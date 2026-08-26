package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// MUL-6704, second half. Moving an agent to another runtime never rewrote
// agent_task_queue.runtime_id, and since #7571 the claim fence requires the two
// to agree — so anything already queued became invisible to the new machine and
// refused by the old one. It then sat in `queued` for the full two-hour TTL and
// failed as `queued_expired` ("task expired in queue"), which describes a busy
// queue, not a rebind.

// TestUpdateAgentRebind_SettlesStrandedQueuedTask pins both halves of the
// decision: settle what can no longer be claimed, and leave what is already
// running alone.
func TestUpdateAgentRebind_SettlesStrandedQueuedTask(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	oldRuntimeID := dbfx.Runtime(t, "Rebind Old Runtime")
	newRuntimeID := dbfx.Runtime(t, "Rebind New Runtime")
	agentID := dbfx.Agent(t, "Rebind Agent", oldRuntimeID)

	queued := insertFixtureTask(t, ctx, oldRuntimeID, agentID, "queued", false)
	waiting := insertFixtureTask(t, ctx, oldRuntimeID, agentID, "waiting_local_directory", false)
	running := insertFixtureTask(t, ctx, oldRuntimeID, agentID, "running", false)

	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/agents/"+agentID, map[string]any{"runtime_id": newRuntimeID})
	req = withURLParam(req, "id", agentID)
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent rebind: got %d, want 200: %s", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		name   string
		taskID string
		status string
		reason string
	}{
		{"queued row is unclaimable after the move", queued, "cancelled", string(taskfailure.ReasonAgentRuntimeChanged)},
		{"waiting row is unclaimable after the move", waiting, "cancelled", string(taskfailure.ReasonAgentRuntimeChanged)},
		// Already handed to the old machine and executing there. CompleteAgentTask
		// does not check the binding, so it still finishes correctly — cancelling
		// it would throw away work the user never asked to stop.
		{"running row keeps executing on the old runtime", running, "running", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status string
			var reason, errText pgtype.Text
			dbfx.QueryRow(t, `SELECT status, failure_reason, error FROM agent_task_queue WHERE id = $1`, tc.taskID).
				Scan(&status, &reason, &errText)
			if status != tc.status {
				t.Fatalf("status = %q, want %q", status, tc.status)
			}
			if tc.reason == "" {
				return
			}
			if reason.String != tc.reason {
				t.Fatalf("failure_reason = %q, want %q", reason.String, tc.reason)
			}
			if errText.String != RebindStrandedTaskError {
				t.Fatalf("error text = %q, want the shared rebind sentence", errText.String)
			}
		})
	}

	// A no-op resubmit of the current runtime is not a rebind: a PATCH-as-PUT
	// client echoing the unchanged runtime_id back must not cancel anything.
	survivor := insertFixtureTask(t, ctx, newRuntimeID, agentID, "queued", false)
	w = httptest.NewRecorder()
	req = newRequest("PATCH", "/api/agents/"+agentID, map[string]any{"runtime_id": newRuntimeID})
	req = withURLParam(req, "id", agentID)
	testHandler.UpdateAgent(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateAgent no-op runtime resubmit: got %d, want 200: %s", w.Code, w.Body.String())
	}
	var survivorStatus string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, survivor).Scan(&survivorStatus)
	if survivorStatus != "queued" {
		t.Fatalf("no-op resubmit cancelled a live task (status %q)", survivorStatus)
	}
}

// TestUpdateAgentRebind_RefusesForeignPrivateRuntime is decision A in force: the
// gate that decides whether an agent may live on a private machine is the AGENT
// OWNER, not the operator. The workspace owner running this test may edit the
// agent and owns the runtime, and it is still refused, because the claim fence
// would refuse every task it produced.
func TestUpdateAgentRebind_RefusesForeignPrivateRuntime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	privateRuntimeID := dbfx.Runtime(t, "Rebind Private Runtime")
	publicRuntimeID := dbfx.Runtime(t, "Rebind Public Runtime", testutil.Cols{"visibility": "public"})
	teammateID := dbfx.User(t, "Rebind Teammate", "rebind-teammate-"+privateRuntimeID+"@multica.ai")
	dbfx.Member(t, testWorkspaceID, teammateID, "member")
	foreignAgentID := dbfx.Agent(t, "Rebind Foreign Agent", publicRuntimeID, testutil.Cols{"owner_id": teammateID})

	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/agents/"+foreignAgentID, map[string]any{"runtime_id": privateRuntimeID})
	req = withURLParam(req, "id", foreignAgentID)
	testHandler.UpdateAgent(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("binding a teammate's agent onto my private runtime: got %d, want 403: %s", w.Code, w.Body.String())
	}
	var boundRuntime string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, foreignAgentID).Scan(&boundRuntime)
	if boundRuntime != publicRuntimeID {
		t.Fatalf("refused rebind must not move the agent; runtime_id = %s", boundRuntime)
	}
}

// TestCancelStaleMismatchedDispatchedTasks covers the sweeper arm. A row the old
// daemon claimed and never started cannot be reclaimed after a rebind (the
// reclaim queries carry the same fence), so before this it drifted until
// FailStaleTasks called it a `timeout` — blaming a machine that was never stuck.
func TestCancelStaleMismatchedDispatchedTasks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	oldRuntimeID := dbfx.Runtime(t, "Sweeper Old Runtime")
	newRuntimeID := dbfx.Runtime(t, "Sweeper New Runtime")
	// The agent lives on the NEW runtime; the rows below are still pinned to the
	// old one, which is what "mismatched" means.
	agentID := dbfx.Agent(t, "Sweeper Agent", newRuntimeID)

	stale := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":               oldRuntimeID,
		"status":                   "dispatched",
		"dispatched_at":            testutil.Raw("now() - interval '10 minutes'"),
		"prepare_lease_expires_at": testutil.Raw("now() - interval '5 minutes'"),
	})
	// Claimed seconds ago: the daemon may still be preparing it, so the sweeper
	// must keep its hands off.
	fresh := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":    oldRuntimeID,
		"status":        "dispatched",
		"dispatched_at": testutil.Raw("now()"),
	})
	// Live lease: the daemon is actively renewing it between claim and StartTask.
	leased := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":               oldRuntimeID,
		"status":                   "dispatched",
		"dispatched_at":            testutil.Raw("now() - interval '10 minutes'"),
		"prepare_lease_expires_at": testutil.Raw("now() + interval '5 minutes'"),
	})
	// Matching binding: an ordinary in-flight task on the agent's own runtime.
	matched := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id":               newRuntimeID,
		"status":                   "dispatched",
		"dispatched_at":            testutil.Raw("now() - interval '10 minutes'"),
		"prepare_lease_expires_at": testutil.Raw("now() - interval '5 minutes'"),
	})
	// Running on the old machine: finishes there, never swept.
	runningElsewhere := dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": oldRuntimeID,
		"status":     "running",
	})

	cancelled, err := testHandler.Queries.CancelStaleMismatchedDispatchedTasks(ctx, db.CancelStaleMismatchedDispatchedTasksParams{
		ClaimRecoverySecs: 90,
		MaxPerTick:        100,
		Error:             pgtype.Text{String: RebindStrandedTaskError, Valid: true},
		FailureReason:     pgtype.Text{String: string(taskfailure.ReasonAgentRuntimeChanged), Valid: true},
	})
	if err != nil {
		t.Fatalf("CancelStaleMismatchedDispatchedTasks: %v", err)
	}
	if len(cancelled) != 1 || uuidToString(cancelled[0].ID) != stale {
		t.Fatalf("swept %d rows, want exactly the stale mismatched one (%s): %+v", len(cancelled), stale, cancelled)
	}
	if cancelled[0].FailureReason.String != string(taskfailure.ReasonAgentRuntimeChanged) {
		t.Fatalf("failure_reason = %q, want %q", cancelled[0].FailureReason.String, taskfailure.ReasonAgentRuntimeChanged)
	}

	for _, id := range []string{fresh, leased, matched, runningElsewhere} {
		var status string
		dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, id).Scan(&status)
		if status == "cancelled" {
			t.Fatalf("task %s must not be swept", id)
		}
	}
}
