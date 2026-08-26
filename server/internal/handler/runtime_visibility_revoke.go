package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// Reclaiming a shared machine (public → private) is the second half of the
// runtime-access work started in #7571 (MUL-6697). That PR closed the execution
// door: a private runtime can no longer RUN an agent belonging to anyone else.
// It deliberately left the state behind it untouched, so a revoke produced
// agents that looked bound but could never run, tasks nothing would ever claim,
// and Autopilots that appended a doomed run every tick — all with no user-facing
// explanation.
//
// This file is that cleanup, modelled on the runtime-delete cascade because the
// problem is the same shape: a snapshot the user confirms, then one transaction
// that unbinds, pauses, cancels and asserts drained, then one post-commit
// broadcast. The differences are all consequences of the runtime SURVIVING:
//
//   - Only FOREIGN agents are affected. The owner's own agents keep running, so
//     the cancel is by agent_id with an EMPTY runtime_ids list — passing the
//     runtime would kill the owner's own work on their own machine.
//   - Task history stays pinned to the runtime (no UnbindTasksFromRuntime): the
//     row still exists, and agent_task_queue_active_requires_runtime only
//     constrains active rows.
//   - `kind = 'system'` carriers keep their binding instead of being deleted
//     (see the retained-set comment below).
const (
	// runtimeVisibilityHasForeignAgentsCode is returned by the plain PATCH when
	// flipping to private would affect agents that are not the owner's. The
	// client shows the impact dialog and calls the confirm endpoint.
	runtimeVisibilityHasForeignAgentsCode = "runtime_visibility_has_foreign_agents"
	// runtimeVisibilityPlanChangedCode is returned by the confirm endpoint when
	// the affected set moved between dialog-open and confirm. Zero writes.
	runtimeVisibilityPlanChangedCode = "runtime_visibility_plan_changed"
	// runtimeVisibilityNotDrainedCode mirrors runtime_delete_not_drained: the
	// cancel left a non-terminal row behind, so the transaction is abandoned
	// rather than committing a half-revoked state.
	runtimeVisibilityNotDrainedCode = "runtime_visibility_not_drained"
)

// User-visible copy stored on the cancelled task rows. It lands in
// agent_task_queue.error, next to the machine-readable failure_reason, and is
// what a user reading the task sees.
//
// RebindStrandedTaskError is exported because two paths write it: UpdateAgent for
// the rows it can settle synchronously, and the sweeper for a dispatched row the
// reclaim queries have abandoned. One string keeps the two indistinguishable to
// the reader, which is correct — the cause is the same.
const (
	revokeUnboundTaskError  = "The runtime this agent was using was made private by its owner, so the agent was unbound. Bind the agent to a runtime you can use and retry."
	revokeRetainedTaskError = "The runtime this agent runs on was made private by its owner and no longer permits this agent. Ask the owner to share it again, or move the agent to another runtime."

	RebindStrandedTaskError = "The agent moved to another runtime before this task started, so it could no longer be claimed. Retry it to run on the agent's current runtime."
)

// runtimeRevokePlan is what a public → private revoke will do, computed from the
// agents currently bound to the runtime whose owner is not the runtime owner.
type runtimeRevokePlan struct {
	// UnboundAgents are the active user agents that lose their binding. This is
	// the set the user confirms (expected_active_agent_ids) — it is what the
	// dialog can actually name, and what the confirming user is accountable for.
	UnboundAgents []db.Agent
	// ArchivedCount counts foreign archived user agents. They are unbound too
	// (an archived agent is still the user's data and would otherwise stay
	// bound to a machine that refuses it), but they are not in the confirmed
	// set: they are invisible in the UI, and including them would make every
	// older client's snapshot mismatch.
	ArchivedCount int
	// RetainedSystemCount counts foreign `kind = 'system'` carriers — Agent
	// Builder sessions, in practice. They KEEP their binding: a system agent has
	// no UI to rebind it, so unbinding one strands a row nobody can repair, and
	// deleting it (what the runtime-delete path does) would destroy the user's
	// builder conversation. Their in-flight tasks are cancelled and admission
	// refuses new ones with runtime_access_revoked, so nothing runs; the user
	// repairs it by switching the builder session's runtime.
	RetainedSystemCount int
	// MikaAffected reports that one of the unbound agents is this workspace's
	// Mika. Mika is `kind = 'user'` (CreateSystemUserAgent is explicit about
	// that), so she is unbound like any other agent — and because there is one
	// Mika per workspace, that stops her for EVERYONE, not just her owner. The
	// dialog has to say so.
	MikaAffected bool
}

func (p runtimeRevokePlan) empty() bool {
	return len(p.UnboundAgents) == 0 && p.ArchivedCount == 0 && p.RetainedSystemCount == 0
}

// splitForeignAgents turns the raw foreign-agent set into the plan. Shared by
// the pre-check (unlocked read) and the confirm transaction (locked read) so the
// two can never disagree about what the plan means.
func splitForeignAgents(agents []db.Agent) (plan runtimeRevokePlan, unboundIDs, retainedIDs []pgtype.UUID) {
	for _, a := range agents {
		if a.Kind == "system" {
			plan.RetainedSystemCount++
			retainedIDs = append(retainedIDs, a.ID)
			continue
		}
		unboundIDs = append(unboundIDs, a.ID)
		if a.ArchivedAt.Valid {
			plan.ArchivedCount++
			continue
		}
		plan.UnboundAgents = append(plan.UnboundAgents, a)
		if a.SystemKey.Valid && a.SystemKey.String == service.MikaSystemKey {
			plan.MikaAffected = true
		}
	}
	return plan, unboundIDs, retainedIDs
}

// buildRuntimeRevokePlan reads the foreign-agent set for a runtime. q is the
// caller's query handle so the confirm path can pass its transaction.
func (h *Handler) buildRuntimeRevokePlan(ctx context.Context, q *db.Queries, rt db.AgentRuntime) (runtimeRevokePlan, error) {
	agents, err := q.ListForeignAgentsByRuntime(ctx, db.ListForeignAgentsByRuntimeParams{
		RuntimeID: rt.ID,
		OwnerID:   rt.OwnerID,
	})
	if err != nil {
		return runtimeRevokePlan{}, err
	}
	plan, _, _ := splitForeignAgents(agents)
	return plan, nil
}

// runtimeRevokePlanResponse is the 409 body for both codes. Shape mirrors
// runtimeHasActiveAgentsResponse (`error` + `code` + agent list) so clients keep
// one 409 handling pattern, with the revoke-specific counts alongside.
func (h *Handler) runtimeRevokePlanResponse(plan runtimeRevokePlan, code string) map[string]any {
	agents := make([]AgentResponse, len(plan.UnboundAgents))
	for i, a := range plan.UnboundAgents {
		agents[i] = h.agentToResponse(a)
	}
	message := "making this runtime private affects agents that are not yours. Review and confirm the impact first."
	if code == runtimeVisibilityPlanChangedCode {
		message = "the affected agent set changed; please review and confirm again."
	}
	return map[string]any{
		"error":                 message,
		"code":                  code,
		"active_agents":         agents,
		"archived_agent_count":  plan.ArchivedCount,
		"retained_agent_count":  plan.RetainedSystemCount,
		"mika_affected":         plan.MikaAffected,
		"requires_confirmation": true,
	}
}

// revokeAndMakePrivateRequest carries the confirmed snapshot. Same field name and
// semantics as the confirmed-delete endpoint so clients reuse their logic.
type revokeAndMakePrivateRequest struct {
	ExpectedActiveAgentIDs []string `json:"expected_active_agent_ids"`
}

// RevokeAndMakePrivateRuntime is the confirmed public → private revoke:
// POST /api/runtimes/:id/revoke-and-make-private.
//
// Owner-only, like the PATCH it completes (canSetRuntimeVisibility): lending the
// machine out was the owner's decision and so is taking it back — an admin doing
// it for them would be exactly the override MUL-6126 removed.
func (h *Handler) RevokeAndMakePrivateRuntime(w http.ResponseWriter, r *http.Request) {
	runtimeID := chi.URLParam(r, "runtimeId")
	runtimeUUID, ok := parseUUIDOrBadRequest(w, runtimeID, "runtime_id")
	if !ok {
		return
	}

	var req revokeAndMakePrivateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	expected, ok := parseExpectedActiveAgentIDs(req.ExpectedActiveAgentIDs)
	if !ok {
		writeError(w, http.StatusBadRequest, "expected_active_agent_ids must be a list of valid UUIDs")
		return
	}

	rt, err := h.Queries.GetAgentRuntime(r.Context(), runtimeUUID)
	if err != nil {
		writeError(w, http.StatusNotFound, "runtime not found")
		return
	}
	wsID := uuidToString(rt.WorkspaceID)
	member, ok := h.requireWorkspaceMember(w, r, wsID, "runtime not found")
	if !ok {
		return
	}
	if !canSetRuntimeVisibility(member, rt) {
		writeError(w, http.StatusForbidden, "only the runtime owner can change its visibility")
		return
	}
	userID := uuidToString(member.UserID)

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	// Lock order is runtime → agents (by id), identical to DeleteAgentRuntime and
	// revokeAndRemoveMember. Diverging here would let a revoke and a delete of
	// the same runtime deadlock. The runtime lock is FOR UPDATE, which is what
	// blocks a concurrent bind: agent INSERT/UPDATE needs FOR KEY SHARE on this
	// row, and revalidateRuntimeForBind re-reads the row after that wait, so a
	// binder that started with a stale `public` snapshot cannot re-create the
	// state we are about to clean up.
	locked, err := qtx.LockAgentRuntime(r.Context(), rt.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock runtime")
		return
	}
	rt = locked
	if rt.Visibility == "private" {
		// Another confirm already landed. Idempotent success rather than an
		// error: the requested end state holds and nothing is left to tear down.
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to commit transaction")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":            "ok",
			"agents_unbound":    0,
			"tasks_cancelled":   0,
			"autopilots_paused": 0,
			"agents_retained":   0,
		})
		return
	}

	foreign, err := qtx.LockForeignAgentsByRuntime(r.Context(), db.LockForeignAgentsByRuntimeParams{
		RuntimeID: rt.ID,
		OwnerID:   rt.OwnerID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock runtime dependencies")
		return
	}
	plan, unboundIDs, retainedIDs := splitForeignAgents(foreign)
	if !activeAgentSetMatches(plan.UnboundAgents, expected) {
		writeJSON(w, http.StatusConflict, h.runtimeRevokePlanResponse(plan, runtimeVisibilityPlanChangedCode))
		return
	}

	teardown, err := revokeRuntimeVisibility(r.Context(), qtx, rt, unboundIDs, retainedIDs)
	if err != nil {
		if errors.Is(err, errRuntimeNotDrained) {
			slog.Error("runtime visibility revoke aborted: tasks not drained",
				"runtime_id", uuidToString(rt.ID), "error", err)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "the runtime still has tasks in flight; retry in a moment.",
				"code":  runtimeVisibilityNotDrainedCode,
			})
			return
		}
		slog.Error("runtime visibility revoke failed", "runtime_id", uuidToString(rt.ID), "error", err)
		writeError(w, http.StatusInternalServerError, "failed to make runtime private")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit transaction")
		return
	}

	slog.Info("runtime made private, foreign agents revoked",
		"runtime_id", uuidToString(rt.ID),
		"revoked_by", userID,
		"agents_unbound", len(teardown.UnboundAgents),
		"agents_retained", len(retainedIDs),
		"tasks_cancelled", len(teardown.CancelledTasks),
		"autopilots_paused", len(teardown.PausedAutopilots),
	)

	// The runtime row survives, so the trailing runtime event is an update.
	// task:cancelled goes first (and revokes each task's token through
	// captureTaskCancelled), then agent and Autopilot rows.
	h.publishRuntimeTeardown(r.Context(), teardown, wsID, userID, "update")

	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"agents_unbound":    len(teardown.UnboundAgents),
		"tasks_cancelled":   len(teardown.CancelledTasks),
		"autopilots_paused": len(teardown.PausedAutopilots),
		"agents_retained":   len(retainedIDs),
	})
}

// revokeRuntimeVisibility is the whole teardown, inside the caller's
// transaction. Step order follows unbindRuntimeForDelete so the two paths share
// their race-safety reasoning:
//
//  1. Flip the visibility. First, so anything that blocks on our runtime lock
//     and then re-reads the row (revalidateRuntimeForBind, the claim fence)
//     observes `private` the moment we commit.
//  2. Unbind the foreign user agents — owner-filtered, active and archived.
//  3. Pause their Autopilots (direct assignee or led squad) with
//     agent_runtime_required, preserving the configuration for a resume after
//     rebinding. Retained system carriers are not Autopilot assignees.
//  4. Cancel non-terminal tasks: agent-side only, and with a reason per group so
//     the two situations read differently to the user — the unbound agents need
//     a new runtime, the retained carriers need access back.
//  5. Assert drained over the same agent set. A missed status means the cancel
//     query and this predicate disagree, which is a bug: abort with 409 rather
//     than commit a partial revoke.
func revokeRuntimeVisibility(ctx context.Context, qtx *db.Queries, rt db.AgentRuntime, unboundIDs, retainedIDs []pgtype.UUID) (runtimeTeardownResult, error) {
	var out runtimeTeardownResult

	if _, err := qtx.UpdateAgentRuntimeVisibility(ctx, db.UpdateAgentRuntimeVisibilityParams{
		ID:         rt.ID,
		Visibility: "private",
	}); err != nil {
		return out, fmt.Errorf("update visibility: %w", err)
	}

	unbound, err := qtx.UnbindForeignUserAgentsFromRuntime(ctx, db.UnbindForeignUserAgentsFromRuntimeParams{
		RuntimeID: rt.ID,
		OwnerID:   rt.OwnerID,
	})
	if err != nil {
		return out, fmt.Errorf("unbind foreign agents: %w", err)
	}
	out.UnboundAgents = unbound

	paused, err := qtx.PauseAutopilotsByUnboundAgents(ctx, unboundIDs)
	if err != nil {
		return out, fmt.Errorf("pause autopilots: %w", err)
	}
	out.PausedAutopilots = paused

	if len(unboundIDs) > 0 {
		cancelled, err := qtx.CancelAgentTasksByAgentsWithReason(ctx, db.CancelAgentTasksByAgentsWithReasonParams{
			AgentIds:      unboundIDs,
			Error:         pgtype.Text{String: revokeUnboundTaskError, Valid: true},
			FailureReason: pgtype.Text{String: string(taskfailure.ReasonAgentRuntimeRequired), Valid: true},
		})
		if err != nil {
			return out, fmt.Errorf("cancel unbound agent tasks: %w", err)
		}
		out.CancelledTasks = append(out.CancelledTasks, cancelled...)
	}
	if len(retainedIDs) > 0 {
		cancelled, err := qtx.CancelAgentTasksByAgentsWithReason(ctx, db.CancelAgentTasksByAgentsWithReasonParams{
			AgentIds:      retainedIDs,
			Error:         pgtype.Text{String: revokeRetainedTaskError, Valid: true},
			FailureReason: pgtype.Text{String: string(taskfailure.ReasonRuntimeAccessRevoked), Valid: true},
		})
		if err != nil {
			return out, fmt.Errorf("cancel retained agent tasks: %w", err)
		}
		out.CancelledTasks = append(out.CancelledTasks, cancelled...)
	}

	// Agent-side only: runtime_ids stays empty so the owner's own tasks on their
	// own machine are untouched.
	undrained, err := qtx.CountUndrainedTasksByRuntimeOrAgent(ctx, db.CountUndrainedTasksByRuntimeOrAgentParams{
		RuntimeIds: []pgtype.UUID{},
		AgentIds:   append(append([]pgtype.UUID{}, unboundIDs...), retainedIDs...),
	})
	if err != nil {
		return out, fmt.Errorf("count undrained tasks: %w", err)
	}
	if undrained > 0 {
		return out, fmt.Errorf("%w: %d", errRuntimeNotDrained, undrained)
	}
	return out, nil
}
