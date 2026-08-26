package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/taskfailure"
)

// MUL-6704. #7571 stopped a reclaimed private runtime from EXECUTING other
// people's agents; these tests pin what happens to the state that reclaim leaves
// behind — the bindings, the queued work, the automations — and the two
// invariants that keep the teardown from doing collateral damage: the owner's own
// agents must be untouched, and the confirmed plan must still be the live one.

// TestSplitForeignAgents_Classification is the pure half: which of the foreign
// agents get unbound, which keep their binding, and when the dialog has to warn
// about Mika.
func TestSplitForeignAgents_Classification(t *testing.T) {
	owner := mustUUID(t, "11111111-1111-4111-8111-111111111111")
	other := mustUUID(t, "22222222-2222-4222-8222-222222222222")

	agents := []db.Agent{
		{ID: mustUUID(t, "aaaaaaaa-0000-4000-8000-000000000001"), Kind: "user", OwnerID: other, Name: "teammate agent"},
		{ID: mustUUID(t, "aaaaaaaa-0000-4000-8000-000000000002"), Kind: "user", OwnerID: other, Name: "archived teammate agent",
			ArchivedAt: pgtype.Timestamptz{Valid: true}},
		{ID: mustUUID(t, "aaaaaaaa-0000-4000-8000-000000000003"), Kind: "system", OwnerID: other, Name: "builder carrier",
			SystemKey: pgtype.Text{String: "agent_builder:abc", Valid: true}},
		{ID: mustUUID(t, "aaaaaaaa-0000-4000-8000-000000000004"), Kind: "user", OwnerID: other, Name: "Mika",
			SystemKey: pgtype.Text{String: service.MikaSystemKey, Valid: true}},
	}

	plan, unboundIDs, retainedIDs := splitForeignAgents(agents)

	if len(plan.UnboundAgents) != 2 {
		t.Fatalf("confirmed set = %d agents, want 2 (the two ACTIVE user agents)", len(plan.UnboundAgents))
	}
	if plan.ArchivedCount != 1 {
		t.Fatalf("archived count = %d, want 1", plan.ArchivedCount)
	}
	if plan.RetainedSystemCount != 1 {
		t.Fatalf("retained system count = %d, want 1", plan.RetainedSystemCount)
	}
	// Mika is kind='user' (CreateSystemUserAgent is explicit about it), so she is
	// unbound like anyone else — and because there is one per workspace, the
	// dialog must say the whole workspace loses her.
	if !plan.MikaAffected {
		t.Fatalf("mika_affected = false; a kind='user' agent with the mika system_key must raise the workspace-wide warning")
	}
	// The unbind/cancel sets are id lists, and they must cover the archived row
	// too: leaving it bound to a machine that refuses it is the stuck state this
	// teardown exists to remove.
	if len(unboundIDs) != 3 {
		t.Fatalf("unbound ids = %d, want 3 (active + archived user agents)", len(unboundIDs))
	}
	if len(retainedIDs) != 1 {
		t.Fatalf("retained ids = %d, want 1", len(retainedIDs))
	}
	if plan.empty() {
		t.Fatalf("plan.empty() = true for a non-empty plan")
	}

	// An owner-only runtime has nothing to tear down: the PATCH must go straight
	// through rather than 409 with an empty dialog.
	ownPlan, ownUnbound, ownRetained := splitForeignAgents(nil)
	if !ownPlan.empty() || len(ownUnbound) != 0 || len(ownRetained) != 0 {
		t.Fatalf("empty foreign set must produce an empty plan, got %+v", ownPlan)
	}
	_ = owner
}

// TestRuntimeAllowsAgentOwner_Pure fences the predicate every layer shares. It
// mirrors the SQL claim fence in #7571 and the post-claim recheck: get these
// three wrong in different places and an agent can be bound to a machine that
// will never run it.
func TestRuntimeAllowsAgentOwner_Pure(t *testing.T) {
	owner := mustUUID(t, "11111111-1111-4111-8111-111111111111")
	other := mustUUID(t, "22222222-2222-4222-8222-222222222222")

	cases := []struct {
		name  string
		rt    db.AgentRuntime
		agent pgtype.UUID
		want  bool
	}{
		{"public runtime runs any owner's agent", db.AgentRuntime{Visibility: "public", OwnerID: owner}, other, true},
		{"private runtime runs its owner's agent", db.AgentRuntime{Visibility: "private", OwnerID: owner}, owner, true},
		{"private runtime refuses a foreign agent", db.AgentRuntime{Visibility: "private", OwnerID: owner}, other, false},
		{"private runtime refuses an ownerless agent", db.AgentRuntime{Visibility: "private", OwnerID: owner}, pgtype.UUID{}, false},
		// An ownerless runtime is left to the existing MUL-3292 handling (no
		// token minter → the claim path cancels); this predicate must not
		// relabel that as an access problem.
		{"ownerless runtime is not this predicate's call", db.AgentRuntime{Visibility: "private"}, other, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := service.RuntimeAllowsAgentOwner(tc.rt, tc.agent); got != tc.want {
				t.Fatalf("RuntimeAllowsAgentOwner = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUpdateRuntimeVisibility_RefusesWithPlan: the plain PATCH must not perform a
// teardown as a side effect of a field write. It refuses with the impact plan and
// leaves the runtime public, so the user's next step is an explicit confirmation.
func TestUpdateRuntimeVisibility_RefusesWithPlan(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Revoke Plan Runtime")
	foreignAgentID := dbfx.Agent(t, "Revoke Plan Foreign Agent", runtimeID, ownedBy(foreignUserID))

	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/runtimes/"+runtimeID, map[string]any{"visibility": "private"})
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("PATCH visibility=private: got %d, want 409: %s", w.Code, w.Body.String())
	}
	var body struct {
		Code         string `json:"code"`
		ActiveAgents []struct {
			ID string `json:"id"`
		} `json:"active_agents"`
		MikaAffected bool `json:"mika_affected"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if body.Code != runtimeVisibilityHasForeignAgentsCode {
		t.Fatalf("code = %q, want %q", body.Code, runtimeVisibilityHasForeignAgentsCode)
	}
	if len(body.ActiveAgents) != 1 || body.ActiveAgents[0].ID != foreignAgentID {
		t.Fatalf("409 must carry the affected agent so the dialog needs no second round trip, got %+v", body.ActiveAgents)
	}

	var visibility string
	dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&visibility)
	if visibility != "public" {
		t.Fatalf("visibility = %q after a refused PATCH, want unchanged 'public'", visibility)
	}
}

// TestUpdateRuntimeVisibility_PlanDisclosesOnlyIdAndName is the negative test for
// the disclosure boundary. The machine owner here is a plain workspace member who
// does not own — and has no read access to — the private agent in the plan, yet
// merely ATTEMPTING to make their own runtime private renders it. If this body
// ever grows to the full AgentResponse again, that attempt hands out the
// teammate's instructions, runtime config, MCP servers and Composio allowlist.
func TestUpdateRuntimeVisibility_PlanDisclosesOnlyIdAndName(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Revoke Disclosure Runtime")
	foreignAgentID := dbfx.Agent(t, "Revoke Disclosure Foreign Agent", runtimeID, testutil.Cols{
		"owner_id":     foreignUserID,
		"visibility":   "private",
		"instructions": "SENTINEL_INSTRUCTIONS do not disclose",
		"mcp_config": testutil.Raw(
			`'{"servers":{"secret":{"command":"SENTINEL_MCP_COMMAND"}}}'::jsonb`),
		"runtime_config":             testutil.Raw(`'{"gateway":{"token":"SENTINEL_TOKEN"}}'::jsonb`),
		"composio_toolkit_allowlist": testutil.Raw(`ARRAY['sentinel_toolkit']::text[]`),
	})

	w := httptest.NewRecorder()
	req := newRequest("PATCH", "/api/runtimes/"+runtimeID, map[string]any{"visibility": "private"})
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.UpdateAgentRuntime(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("PATCH visibility=private: got %d, want 409: %s", w.Code, w.Body.String())
	}

	raw := w.Body.String()
	for _, sentinel := range []string{
		"SENTINEL_INSTRUCTIONS",
		"SENTINEL_MCP_COMMAND",
		"SENTINEL_TOKEN",
		"sentinel_toolkit",
		// Field names too: an empty or redacted value still means the shape grew
		// back, and the next config written to it would leak.
		"instructions",
		"mcp_config",
		"runtime_config",
		"composio_toolkit_allowlist",
	} {
		if strings.Contains(raw, sentinel) {
			t.Fatalf("409 plan discloses %q; it must carry only id and name.\nbody: %s", sentinel, raw)
		}
	}

	// And it still says enough for the confirmation to be meaningful.
	var body struct {
		ActiveAgents []map[string]any `json:"active_agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if len(body.ActiveAgents) != 1 {
		t.Fatalf("active_agents = %d, want 1", len(body.ActiveAgents))
	}
	entry := body.ActiveAgents[0]
	if len(entry) != 2 {
		t.Fatalf("agent entry has %d fields (%v), want exactly id + name", len(entry), entry)
	}
	if entry["id"] != foreignAgentID || entry["name"] != "Revoke Disclosure Foreign Agent" {
		t.Fatalf("agent entry = %v, want the affected agent's id and name", entry)
	}
}

// TestRevokeAndMakePrivate_TearsDownForeignState is the main path: one confirmed
// call has to leave no half-revoked state behind — the foreign agent unbound, its
// work cancelled with a reason, its automation paused — while the owner's own
// agent and work continue untouched.
func TestRevokeAndMakePrivate_TearsDownForeignState(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Revoke Teardown Runtime")
	foreignAgentID := dbfx.Agent(t, "Revoke Teardown Foreign", runtimeID, ownedBy(foreignUserID))
	ownAgentID := dbfx.Agent(t, "Revoke Teardown Own", runtimeID)

	foreignQueued := insertFixtureTask(t, ctx, runtimeID, foreignAgentID, "queued", false)
	foreignRunning := insertFixtureTask(t, ctx, runtimeID, foreignAgentID, "running", false)
	ownQueued := insertFixtureTask(t, ctx, runtimeID, ownAgentID, "queued", false)

	autopilotID := dbfx.Insert(t, "autopilot", testutil.Cols{
		"workspace_id":    testWorkspaceID,
		"title":           "revoke teardown autopilot",
		"assignee_type":   "agent",
		"assignee_id":     foreignAgentID,
		"status":          "active",
		"execution_mode":  "run_only",
		"created_by_type": "member",
		"created_by_id":   foreignUserID,
	})

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/runtimes/"+runtimeID+"/revoke-and-make-private",
		map[string]any{"expected_active_agent_ids": []string{foreignAgentID}})
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.RevokeAndMakePrivateRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("RevokeAndMakePrivateRuntime: got %d, want 200: %s", w.Code, w.Body.String())
	}

	var visibility string
	dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&visibility)
	if visibility != "private" {
		t.Fatalf("visibility = %q, want 'private'", visibility)
	}

	var foreignBound, ownBound bool
	dbfx.QueryRow(t, `SELECT runtime_id IS NOT NULL FROM agent WHERE id = $1`, foreignAgentID).Scan(&foreignBound)
	dbfx.QueryRow(t, `SELECT runtime_id IS NOT NULL FROM agent WHERE id = $1`, ownAgentID).Scan(&ownBound)
	if foreignBound {
		t.Fatalf("the foreign agent must be unbound")
	}
	if !ownBound {
		// The regression this guards: UnbindUserAgentsFromRuntime has no owner
		// filter, so reusing it here would unbind the owner's own agents —
		// reclaiming your machine would break your own agents first.
		t.Fatalf("the runtime owner's own agent must stay bound")
	}

	for _, tc := range []struct {
		taskID string
		status string
		reason string
	}{
		{foreignQueued, "cancelled", string(taskfailure.ReasonAgentRuntimeRequired)},
		{foreignRunning, "cancelled", string(taskfailure.ReasonAgentRuntimeRequired)},
		{ownQueued, "queued", ""},
	} {
		var status string
		var reason, errText pgtype.Text
		dbfx.QueryRow(t, `SELECT status, failure_reason, error FROM agent_task_queue WHERE id = $1`, tc.taskID).
			Scan(&status, &reason, &errText)
		if status != tc.status {
			t.Fatalf("task %s status = %q, want %q", tc.taskID, status, tc.status)
		}
		if tc.reason == "" {
			continue
		}
		if reason.String != tc.reason {
			t.Fatalf("task %s failure_reason = %q, want %q", tc.taskID, reason.String, tc.reason)
		}
		if errText.String == "" {
			t.Fatalf("task %s has a machine reason but no sentence a user can read", tc.taskID)
		}
	}

	var autopilotStatus string
	var pauseReason pgtype.Text
	dbfx.QueryRow(t, `SELECT status, pause_reason FROM autopilot WHERE id = $1`, autopilotID).Scan(&autopilotStatus, &pauseReason)
	if autopilotStatus != "paused" || pauseReason.String != "agent_runtime_required" {
		t.Fatalf("autopilot = (%q, %q), want (paused, agent_runtime_required): an active schedule would append one doomed run per tick",
			autopilotStatus, pauseReason.String)
	}
}

// TestRevokeAndMakePrivate_RefusesStalePlan: the set the user confirmed must be
// the set the server tears down. A teammate binding another agent while the
// dialog is open has to force a re-confirmation, with zero writes in between.
func TestRevokeAndMakePrivate_RefusesStalePlan(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Revoke Stale Plan Runtime")
	firstAgentID := dbfx.Agent(t, "Revoke Stale First", runtimeID, ownedBy(foreignUserID))
	dbfx.Agent(t, "Revoke Stale Second", runtimeID, ownedBy(foreignUserID))

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/runtimes/"+runtimeID+"/revoke-and-make-private",
		map[string]any{"expected_active_agent_ids": []string{firstAgentID}})
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.RevokeAndMakePrivateRuntime(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("stale plan: got %d, want 409: %s", w.Code, w.Body.String())
	}
	var body struct {
		Code         string `json:"code"`
		ActiveAgents []struct {
			ID string `json:"id"`
		} `json:"active_agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if body.Code != runtimeVisibilityPlanChangedCode {
		t.Fatalf("code = %q, want %q", body.Code, runtimeVisibilityPlanChangedCode)
	}
	if len(body.ActiveAgents) != 2 {
		t.Fatalf("the refusal must carry the LATEST plan so the dialog can re-render it, got %d agents", len(body.ActiveAgents))
	}

	var visibility string
	var boundAgents int
	dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&visibility)
	dbfx.QueryRow(t, `SELECT count(*) FROM agent WHERE runtime_id = $1`, runtimeID).Scan(&boundAgents)
	if visibility != "public" || boundAgents != 2 {
		t.Fatalf("a refused confirmation must write nothing: visibility=%q boundAgents=%d", visibility, boundAgents)
	}
}

// TestRevokeAndMakePrivate_RetainsSystemCarrier: an Agent Builder carrier is
// invisible infrastructure with no rebind affordance in the agent UI, so it keeps
// its binding (unbinding strands it, deleting it destroys the user's builder
// conversation). Its in-flight work is still cancelled, and admission refuses new
// work — see TestAgentReadiness_BlocksRevokedRuntimeAccess.
func TestRevokeAndMakePrivate_RetainsSystemCarrier(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Revoke Carrier Runtime")
	carrierID := dbfx.Agent(t, "Revoke Carrier", runtimeID, testutil.Cols{
		"owner_id":   foreignUserID,
		"kind":       "system",
		"system_key": "agent_builder:revoke-test",
	})
	carrierTask := insertFixtureTask(t, ctx, runtimeID, carrierID, "queued", false)

	w := httptest.NewRecorder()
	req := newRequest("POST", "/api/runtimes/"+runtimeID+"/revoke-and-make-private",
		map[string]any{"expected_active_agent_ids": []string{}})
	req = withURLParam(req, "runtimeId", runtimeID)
	testHandler.RevokeAndMakePrivateRuntime(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("RevokeAndMakePrivateRuntime: got %d, want 200: %s", w.Code, w.Body.String())
	}

	var carrierBound bool
	dbfx.QueryRow(t, `SELECT runtime_id IS NOT NULL FROM agent WHERE id = $1`, carrierID).Scan(&carrierBound)
	if !carrierBound {
		t.Fatalf("a kind='system' carrier must keep its binding; unbound it is unrepairable")
	}

	var status string
	var reason pgtype.Text
	dbfx.QueryRow(t, `SELECT status, failure_reason FROM agent_task_queue WHERE id = $1`, carrierTask).Scan(&status, &reason)
	if status != "cancelled" || reason.String != string(taskfailure.ReasonRuntimeAccessRevoked) {
		t.Fatalf("carrier task = (%q, %q), want (cancelled, %s)", status, reason.String, taskfailure.ReasonRuntimeAccessRevoked)
	}
}

// publicRuntimeWithForeignAgent creates a PUBLIC runtime owned by the test user
// plus a second workspace member to own the "someone else's agent" side of these
// tests. Returns the runtime id and the other member's user id.
func publicRuntimeWithForeignAgent(t *testing.T, ctx context.Context, name string) (string, string) {
	t.Helper()
	runtimeID := dbfx.Runtime(t, name, testutil.Cols{"visibility": "public"})
	otherUserID := dbfx.User(t, name+" Teammate", "teammate-"+runtimeID+"@multica.ai")
	dbfx.Member(t, testWorkspaceID, otherUserID, "member")
	return runtimeID, otherUserID
}

// testutilCols is shorthand for "an agent owned by someone else".
func ownedBy(ownerID string) testutil.Cols {
	return testutil.Cols{"owner_id": ownerID}
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	u := parseUUID(s)
	if !u.Valid {
		t.Fatalf("parse uuid %q", s)
	}
	return u
}
