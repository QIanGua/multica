package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDeleteWorkspace_CollectsTasksThroughEveryOwnershipPath pins the contract
// that MUL-5999's plan fix must not break: agent_task_queue has no workspace_id,
// and nothing enforces that a task's agent, runtime and issue share a workspace,
// so teardown has to sweep all three paths.
//
// The fixture builds one task per path — reachable only via agent, only via
// issue, and only via runtime — plus a neighbour-workspace task that must
// survive. It also pins the task_token simplification: a token whose
// workspace_id points at the neighbour but whose task belongs to the deleted
// workspace is no longer matched by the explicit DELETE, and must therefore be
// removed by the task_id ON DELETE CASCADE.
func TestDeleteWorkspace_CollectsTasksThroughEveryOwnershipPath(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	const victimSlug = "handler-tests-delete-paths-victim"
	const neighbourSlug = "handler-tests-delete-paths-neighbour"
	_, _ = testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = ANY($1::text[])`,
		[]string{victimSlug, neighbourSlug})

	newWorkspace := func(name, slug string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
INSERT INTO workspace (name, slug) VALUES ($1, $2) RETURNING id
`, name, slug).Scan(&id); err != nil {
			t.Fatalf("create workspace %s: %v", slug, err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, id)
		})
		return id
	}

	victimID := newWorkspace("Delete Paths Victim", victimSlug)
	neighbourID := newWorkspace("Delete Paths Neighbour", neighbourSlug)

	if _, err := testPool.Exec(ctx, `
INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
`, victimID, testUserID); err != nil {
		t.Fatalf("create owner member: %v", err)
	}

	newRuntime := func(wsID, name string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
INSERT INTO agent_runtime (workspace_id, name, runtime_mode, provider, status, device_info, metadata, owner_id)
VALUES ($1, $2, 'cloud', 'delete-test', 'offline', '', '{}'::jsonb, $3)
RETURNING id
`, wsID, name, testUserID).Scan(&id); err != nil {
			t.Fatalf("create runtime %s: %v", name, err)
		}
		return id
	}
	newAgent := func(wsID, runtimeID, name string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
INSERT INTO agent (workspace_id, name, runtime_mode, runtime_config, runtime_id, owner_id)
VALUES ($1, $2, 'cloud', '{}'::jsonb, $3, $4)
RETURNING id
`, wsID, name, runtimeID, testUserID).Scan(&id); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
		return id
	}
	newIssue := func(wsID, title string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
INSERT INTO issue (workspace_id, title, creator_type, creator_id)
VALUES ($1, $2, 'member', $3)
RETURNING id
`, wsID, title, testUserID).Scan(&id); err != nil {
			t.Fatalf("create issue %s: %v", title, err)
		}
		return id
	}
	newTask := func(agentID, issueID, runtimeID string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at)
VALUES ($1, $2, $3, 'completed', now())
RETURNING id
`, agentID, issueID, runtimeID).Scan(&id); err != nil {
			t.Fatalf("create task: %v", err)
		}
		return id
	}
	newToken := func(hash, taskID, agentID, wsID string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')
RETURNING id
`, hash, taskID, agentID, wsID, testUserID).Scan(&id); err != nil {
			t.Fatalf("create task token %s: %v", hash, err)
		}
		return id
	}

	victimRuntime := newRuntime(victimID, "victim runtime")
	victimAgent := newAgent(victimID, victimRuntime, "victim agent")
	victimIssue := newIssue(victimID, "victim issue")

	neighbourRuntime := newRuntime(neighbourID, "neighbour runtime")
	neighbourAgent := newAgent(neighbourID, neighbourRuntime, "neighbour agent")
	neighbourIssue := newIssue(neighbourID, "neighbour issue")

	// Reachable only through agent_id.
	taskViaAgent := newTask(victimAgent, neighbourIssue, neighbourRuntime)
	// Reachable only through issue_id.
	taskViaIssue := newTask(neighbourAgent, victimIssue, neighbourRuntime)
	// Reachable only through runtime_id.
	taskViaRuntime := newTask(neighbourAgent, neighbourIssue, victimRuntime)
	// Belongs entirely to the neighbour and must survive.
	neighbourTask := newTask(neighbourAgent, neighbourIssue, neighbourRuntime)

	// Token whose own workspace_id points at the neighbour: only the task_id
	// cascade can clean it up once taskViaIssue is deleted.
	crossToken := newToken("mul5999-cross", taskViaIssue, neighbourAgent, neighbourID)
	victimToken := newToken("mul5999-victim", taskViaAgent, victimAgent, victimID)
	neighbourToken := newToken("mul5999-neighbour", neighbourTask, neighbourAgent, neighbourID)

	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM task_token WHERE id = ANY($1::uuid[])`,
			[]string{crossToken, victimToken, neighbourToken})
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = ANY($1::uuid[])`,
			[]string{taskViaAgent, taskViaIssue, taskViaRuntime, neighbourTask})
	})

	// Assert the collector directly. The end-to-end assertions below cannot
	// distinguish a missing ownership path on their own: agent_id, issue_id and
	// runtime_id all carry ON DELETE CASCADE, so deleting the owning row would
	// sweep the task even if teardown never listed it. The explicit graph is
	// what must keep working once those legacy cascades go away, so pin it here.
	func() {
		tx, err := testHandler.TxStarter.Begin(ctx)
		if err != nil {
			t.Fatalf("begin collector tx: %v", err)
		}
		defer tx.Rollback(ctx)

		collected, err := collectWorkspaceTaskIDs(ctx, testHandler.Queries.WithTx(tx), parseUUID(victimID))
		if err != nil {
			t.Fatalf("collectWorkspaceTaskIDs: %v", err)
		}
		got := make(map[string]int, len(collected))
		for _, id := range collected {
			got[uuidToString(id)]++
		}
		for name, id := range map[string]string{
			"agent path":   taskViaAgent,
			"issue path":   taskViaIssue,
			"runtime path": taskViaRuntime,
		} {
			if got[id] != 1 {
				t.Errorf("%s: task %s collected %d times, want exactly 1", name, id, got[id])
			}
		}
		if got[neighbourTask] != 0 {
			t.Errorf("neighbour-only task %s was collected for the victim workspace", neighbourTask)
		}
	}()

	w := httptest.NewRecorder()
	req := newRequest("DELETE", "/api/workspaces/"+victimID, nil)
	req = withURLParam(req, "id", victimID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}

	exists := func(table, id string) bool {
		var found bool
		if err := testPool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&found); err != nil {
			t.Fatalf("check %s %s: %v", table, id, err)
		}
		return found
	}

	for name, id := range map[string]string{
		"task reachable only via agent_id":   taskViaAgent,
		"task reachable only via issue_id":   taskViaIssue,
		"task reachable only via runtime_id": taskViaRuntime,
	} {
		if exists("agent_task_queue", id) {
			t.Errorf("%s survived workspace delete", name)
		}
	}
	for name, id := range map[string]string{
		"token owned by the deleted workspace":        victimToken,
		"token on a deleted task but owned elsewhere": crossToken,
	} {
		if exists("task_token", id) {
			t.Errorf("%s survived workspace delete", name)
		}
	}

	if !exists("agent_task_queue", neighbourTask) {
		t.Error("neighbour workspace task was deleted")
	}
	if !exists("task_token", neighbourToken) {
		t.Error("neighbour workspace task token was deleted")
	}
	if !exists("agent", neighbourAgent) || !exists("issue", neighbourIssue) || !exists("agent_runtime", neighbourRuntime) {
		t.Error("neighbour workspace objects were deleted")
	}
	if exists("workspace", victimID) {
		t.Error("victim workspace still exists")
	}
}
