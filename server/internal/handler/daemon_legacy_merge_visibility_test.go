package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// TestDaemonRegister_LegacyMergeInheritsPublicVisibility guards the third way an
// agent could end up bound to a machine that refuses to run it (MUL-6704).
//
// A daemon that switches from a hostname-derived id to a stable UUID re-registers
// as a NEW runtime row, which defaults to `private`, and the merge then moves
// every agent from the legacy row onto it. If the legacy row was `public` and
// carried teammates' agents, that merge silently un-shared the machine and dragged
// those agents onto a private runtime — where, since #7571, nothing they own can be
// claimed. No dialog, no teardown, no explanation: exactly the "bound but can never
// run" state the rest of this work removes.
//
// The rule this pins: an identity migration is the same machine, so the surviving
// row inherits the sharing the owner had already chosen. Reclaiming it afterwards
// goes through the confirmed revoke like any other machine.
func TestDaemonRegister_LegacyMergeInheritsPublicVisibility(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	const legacyDaemonID = "SharedMachine.local"
	const newDaemonID = "0192a7a0-9ab3-7c3f-9f1c-4a6fe8c4e802"

	// A shared (public) machine under its old hostname identity.
	legacyRuntimeID := dbfx.Runtime(t, "legacy-public-runtime", testutil.Cols{
		"daemon_id":    legacyDaemonID,
		"runtime_mode": "local",
		"provider":     "claude",
		"status":       "offline",
		"device_info":  legacyDaemonID,
		"visibility":   "public",
		"last_seen_at": testutil.Raw("now() - interval '1 hour'"),
	})

	// A teammate's agent running on that shared machine — legal on a public
	// runtime, and the thing the merge must not break.
	teammateID := dbfx.User(t, "Legacy Merge Teammate", "legacy-merge-teammate@multica.ai")
	dbfx.Member(t, testWorkspaceID, teammateID, "member")
	foreignAgentID := dbfx.Agent(t, "legacy-merge-foreign-agent", legacyRuntimeID, testutil.Cols{
		"owner_id": teammateID,
	})

	// The daemon comes back under a stable UUID, declaring the old id as legacy.
	req := newRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id":      testWorkspaceID,
		"daemon_id":         newDaemonID,
		"legacy_daemon_ids": []string{legacyDaemonID},
		"device_name":       "SharedMachine",
		"runtimes": []map[string]any{
			{"name": "shared-runtime", "type": "claude", "version": "1.0.0", "status": "online"},
		},
	})
	w := testutil.Call(t, testHandler.DaemonRegister, req).Want(http.StatusOK)

	var resp map[string]any
	w.JSON(&resp)
	newRuntimeID := resp["runtimes"].([]any)[0].(map[string]any)["id"].(string)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE runtime_id = $1`, newRuntimeID)
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, newRuntimeID)
	})
	if newRuntimeID == legacyRuntimeID {
		t.Fatalf("expected a new runtime row, got the legacy id back")
	}

	var agentRuntimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, foreignAgentID).Scan(&agentRuntimeID)
	if agentRuntimeID != newRuntimeID {
		t.Fatalf("agent not reassigned by the merge: runtime_id=%s, want %s", agentRuntimeID, newRuntimeID)
	}

	var visibility string
	dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, newRuntimeID).Scan(&visibility)
	if visibility != "public" {
		t.Fatalf("merged runtime visibility = %q, want 'public' inherited from the legacy row: a fresh row defaults to private, which would strand every foreign agent the merge just moved onto it", visibility)
	}

	// The invariant the visibility inheritance exists to protect, asserted the way
	// the rest of the system checks it.
	runtime, err := testHandler.Queries.GetAgentRuntime(ctx, parseUUID(newRuntimeID))
	if err != nil {
		t.Fatalf("load merged runtime: %v", err)
	}
	agent, err := testHandler.Queries.GetAgent(ctx, parseUUID(foreignAgentID))
	if err != nil {
		t.Fatalf("load foreign agent: %v", err)
	}
	if !service.RuntimeAllowsAgentOwner(runtime, agent.OwnerID) {
		t.Fatalf("the merged runtime refuses an agent it just absorbed; its work would queue and never be claimed")
	}
	verdict, err := service.AgentReadiness(ctx, testHandler.Queries, agent)
	if err != nil {
		t.Fatalf("AgentReadiness: %v", err)
	}
	if verdict.Blocked() {
		t.Fatalf("admission blocks the migrated agent (reason %q); a daemon restart must not cost a teammate their runtime", verdict.Reason)
	}
}

// TestDaemonRegister_LegacyMergeKeepsPrivateWhenLegacyWasPrivate: inheritance is
// not "always widen". A machine the owner kept private stays private across the
// identity change — the merge must not turn a daemon restart into an accidental
// share of someone's own computer.
func TestDaemonRegister_LegacyMergeKeepsPrivateWhenLegacyWasPrivate(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	const legacyDaemonID = "PrivateMachine.local"
	const newDaemonID = "0192a7a0-9ab3-7c3f-9f1c-4a6fe8c4e803"

	legacyRuntimeID := dbfx.Runtime(t, "legacy-private-runtime", testutil.Cols{
		"daemon_id":    legacyDaemonID,
		"runtime_mode": "local",
		"provider":     "claude",
		"status":       "offline",
		"device_info":  legacyDaemonID,
		"visibility":   "private",
		"last_seen_at": testutil.Raw("now() - interval '1 hour'"),
	})
	ownAgentID := dbfx.Agent(t, "legacy-merge-own-agent", legacyRuntimeID)

	req := newRequest("POST", "/api/daemon/register", map[string]any{
		"workspace_id":      testWorkspaceID,
		"daemon_id":         newDaemonID,
		"legacy_daemon_ids": []string{legacyDaemonID},
		"device_name":       "PrivateMachine",
		"runtimes": []map[string]any{
			{"name": "private-runtime", "type": "claude", "version": "1.0.0", "status": "online"},
		},
	})
	w := testutil.Call(t, testHandler.DaemonRegister, req).Want(http.StatusOK)

	var resp map[string]any
	w.JSON(&resp)
	newRuntimeID := resp["runtimes"].([]any)[0].(map[string]any)["id"].(string)
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent WHERE runtime_id = $1`, newRuntimeID)
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, newRuntimeID)
	})

	var visibility string
	dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, newRuntimeID).Scan(&visibility)
	if visibility != "private" {
		t.Fatalf("merged runtime visibility = %q, want 'private': a private machine must not become shared by restarting its daemon", visibility)
	}
	var agentRuntimeID string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, ownAgentID).Scan(&agentRuntimeID)
	if agentRuntimeID != newRuntimeID {
		t.Fatalf("own agent not reassigned: runtime_id=%s, want %s", agentRuntimeID, newRuntimeID)
	}
}

// TestRuntimeByID is the pure half: the merge reads the legacy row's pre-merge
// visibility out of the locked batch, so picking the wrong row would silently
// disable the inheritance.
func TestRuntimeByID(t *testing.T) {
	a := mustUUID(t, "11111111-1111-4111-8111-111111111111")
	b := mustUUID(t, "22222222-2222-4222-8222-222222222222")
	rows := []db.AgentRuntime{
		{ID: a, Visibility: "public"},
		{ID: b, Visibility: "private"},
	}
	got, ok := runtimeByID(rows, b)
	if !ok || got.Visibility != "private" {
		t.Fatalf("runtimeByID(b) = (%+v, %v), want the private row", got, ok)
	}
	if _, ok := runtimeByID(rows, mustUUID(t, "33333333-3333-4333-8333-333333333333")); ok {
		t.Fatalf("runtimeByID must report a miss rather than a zero row that reads as private")
	}
}
