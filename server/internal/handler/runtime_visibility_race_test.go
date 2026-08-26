package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// The bind-vs-revoke race, both orderings, on two real connections (MUL-6704).
//
// This is the invariant that is easy to state and easy to lose: a private runtime
// never ends up with a foreign agent bound to it and no teardown. The lock modes
// are what make it subtle. An agent INSERT/UPDATE validates its runtime FK under
// FOR KEY SHARE, and a plain UPDATE of a non-key column (visibility) takes FOR NO
// KEY UPDATE — those two do NOT conflict, so the first version of the
// "nothing to tear down" path could flip a runtime private in parallel with a bind
// that had already passed its check against the `public` snapshot, and both would
// commit.
//
// The fix is a lock on each side that does conflict: the revoke takes FOR UPDATE
// (LockAgentRuntime) before it recounts, and every binder re-reads the row under
// FOR KEY SHARE (LockAgentRuntimeForBind) inside its own transaction. These tests
// hold one side open on a dedicated connection and assert the other side blocks
// and then observes the committed state — not the snapshot it started from.

// waitBlocked gives the handler goroutine long enough to reach its lock wait.
// It is a lower bound on "is definitely blocked", not a timeout: the assertions
// that matter are made after the blocking transaction commits.
const waitBlocked = 250 * time.Millisecond

// handlerResult carries an httptest recorder back from the goroutine driving the
// blocked request.
type handlerResult struct {
	code int
	body string
}

// TestVisibilityRevokeRace_BindHoldsLockFirst: a bind is already in flight,
// holding FOR KEY SHARE on the runtime, when the owner tries to make the machine
// private with (as far as an unlocked read can tell) nothing bound to it.
//
// The PATCH must block on the runtime row rather than flip it, and once the bind
// commits it must see that agent and refuse with the impact plan. The regression
// this pins: with an unlocked read it flipped to private and returned 200, leaving
// a private runtime with a foreign agent bound to it.
func TestVisibilityRevokeRace_BindHoldsLockFirst(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Race Bind First Runtime")
	otherRuntimeID := dbfx.Runtime(t, "Race Bind First Other", testutil.Cols{
		"visibility": "public",
	})
	// The agent starts on another public runtime; the in-flight bind is what moves
	// it onto the machine being reclaimed.
	foreignAgentID := dbfx.Agent(t, "Race Bind First Agent", otherRuntimeID, ownedBy(foreignUserID))

	// Connection 1: the binder. Takes the same FK-shaped lock a real bind takes,
	// writes the new binding, and holds the transaction open.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin bind tx: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback(context.Background())
		}
	}()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM agent_runtime WHERE id = $1 FOR KEY SHARE`, runtimeID); err != nil {
		t.Fatalf("bind tx lock runtime: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent SET runtime_id = $1 WHERE id = $2`, runtimeID, foreignAgentID); err != nil {
		t.Fatalf("bind tx move agent: %v", err)
	}

	// Connection 2: the PATCH, on its own pooled connection.
	done := make(chan handlerResult, 1)
	go func() {
		w := httptest.NewRecorder()
		req := newRequest("PATCH", "/api/runtimes/"+runtimeID, map[string]any{"visibility": "private"})
		req = withURLParam(req, "runtimeId", runtimeID)
		testHandler.UpdateAgentRuntime(w, req)
		done <- handlerResult{code: w.Code, body: w.Body.String()}
	}()

	select {
	case res := <-done:
		t.Fatalf("PATCH completed while a bind held the runtime lock (code %d): it must wait for FOR UPDATE.\nbody: %s",
			res.code, res.body)
	case <-time.After(waitBlocked):
		// Blocked, as required.
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit bind tx: %v", err)
	}
	committed = true

	res := waitForHandler(t, done)
	if res.code != http.StatusConflict {
		t.Fatalf("PATCH after the bind committed: got %d, want 409 with the impact plan.\nbody: %s", res.code, res.body)
	}
	var body struct {
		Code         string `json:"code"`
		ActiveAgents []struct {
			ID string `json:"id"`
		} `json:"active_agents"`
	}
	if err := json.Unmarshal([]byte(res.body), &body); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if body.Code != runtimeVisibilityHasForeignAgentsCode {
		t.Fatalf("code = %q, want %q", body.Code, runtimeVisibilityHasForeignAgentsCode)
	}
	if len(body.ActiveAgents) != 1 || body.ActiveAgents[0].ID != foreignAgentID {
		t.Fatalf("the recount must include the agent that landed during the wait, got %+v", body.ActiveAgents)
	}

	var visibility string
	dbfx.QueryRow(t, `SELECT visibility FROM agent_runtime WHERE id = $1`, runtimeID).Scan(&visibility)
	if visibility != "public" {
		t.Fatalf("visibility = %q; a runtime with a foreign agent must not have been flipped", visibility)
	}
}

// TestVisibilityRevokeRace_RevokeHoldsLockFirst is the mirror image: the revoke
// already holds FOR UPDATE and has written `private` when a bind arrives.
//
// The bind must block, and after the revoke commits it must be refused — not
// proceed on the `public` snapshot it read before the wait. That re-read is the
// point of LockAgentRuntimeForBind: without it the binder blocks (its FK lock
// conflicts), then wakes up and writes anyway.
func TestVisibilityRevokeRace_RevokeHoldsLockFirst(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID, foreignUserID := publicRuntimeWithForeignAgent(t, ctx, "Race Revoke First Runtime")
	otherRuntimeID := dbfx.Runtime(t, "Race Revoke First Other", testutil.Cols{
		"visibility": "public",
	})
	foreignAgentID := dbfx.Agent(t, "Race Revoke First Agent", otherRuntimeID, ownedBy(foreignUserID))

	// Connection 1: the revoke, mid-transaction — runtime locked FOR UPDATE and
	// already flipped, not yet committed.
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin revoke tx: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback(context.Background())
		}
	}()
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM agent_runtime WHERE id = $1 FOR UPDATE`, runtimeID); err != nil {
		t.Fatalf("revoke tx lock runtime: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_runtime SET visibility = 'private' WHERE id = $1`, runtimeID); err != nil {
		t.Fatalf("revoke tx flip visibility: %v", err)
	}

	// Connection 2: a rebind of the foreign agent onto that machine, driven
	// through the real handler.
	done := make(chan handlerResult, 1)
	go func() {
		w := httptest.NewRecorder()
		req := newRequest("PATCH", "/api/agents/"+foreignAgentID, map[string]any{"runtime_id": runtimeID})
		req = withURLParam(req, "id", foreignAgentID)
		testHandler.UpdateAgent(w, req)
		done <- handlerResult{code: w.Code, body: w.Body.String()}
	}()

	select {
	case res := <-done:
		// A 403 here would mean the pre-flight check caught it, not the lock — the
		// runtime was still public when this request read it, so that would mean
		// the fixture, not the fence, decided the outcome.
		t.Fatalf("rebind completed while the revoke held FOR UPDATE (code %d): it must wait.\nbody: %s",
			res.code, res.body)
	case <-time.After(waitBlocked):
		// Blocked, as required.
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit revoke tx: %v", err)
	}
	committed = true

	res := waitForHandler(t, done)
	if res.code != http.StatusForbidden {
		t.Fatalf("rebind after the revoke committed: got %d, want 403 — it must re-read the row, not trust its pre-wait snapshot.\nbody: %s",
			res.code, res.body)
	}

	var boundRuntime string
	dbfx.QueryRow(t, `SELECT runtime_id FROM agent WHERE id = $1`, foreignAgentID).Scan(&boundRuntime)
	if boundRuntime != otherRuntimeID {
		t.Fatalf("agent runtime_id = %s; the refused bind must not have moved it onto the reclaimed machine", boundRuntime)
	}
}

func waitForHandler(t *testing.T, done <-chan handlerResult) handlerResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("blocked request never completed after the holding transaction committed")
		return handlerResult{}
	}
}
