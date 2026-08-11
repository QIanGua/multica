package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// workspaceDeletePathFixture builds the ownership shapes teardown has to handle:
// one task reachable from the victim workspace only through agent_id, one only
// through issue_id, one only through runtime_id, and one that belongs entirely to
// a neighbour workspace and must survive.
//
// The tokens cover the three explicit task_token paths: one owned by the victim
// workspace, one whose workspace_id points at the neighbour while its TASK is the
// victim's, one whose workspace_id points at the neighbour while its AGENT is the
// victim's, and one that is entirely the neighbour's.
type workspaceDeletePathFixture struct {
	victimID    string
	neighbourID string

	victimAgent   string
	victimIssue   string
	victimRuntime string

	neighbourAgent   string
	neighbourIssue   string
	neighbourRuntime string

	taskViaAgent   string
	taskViaIssue   string
	taskViaRuntime string
	neighbourTask  string

	victimToken        string
	crossTaskToken     string
	crossAgentToken    string
	neighbourOnlyToken string
}

func newWorkspaceDeletePathFixture(t *testing.T, slugSuffix string) workspaceDeletePathFixture {
	t.Helper()
	ctx := context.Background()

	victimSlug := "handler-tests-delete-paths-victim-" + slugSuffix
	neighbourSlug := "handler-tests-delete-paths-neighbour-" + slugSuffix
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
	f := workspaceDeletePathFixture{
		victimID:    newWorkspace("Delete Paths Victim "+slugSuffix, victimSlug),
		neighbourID: newWorkspace("Delete Paths Neighbour "+slugSuffix, neighbourSlug),
	}

	if _, err := testPool.Exec(ctx, `
INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')
`, f.victimID, testUserID); err != nil {
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

	f.victimRuntime = newRuntime(f.victimID, "victim runtime")
	f.victimAgent = newAgent(f.victimID, f.victimRuntime, "victim agent")
	f.victimIssue = newIssue(f.victimID, "victim issue")
	f.neighbourRuntime = newRuntime(f.neighbourID, "neighbour runtime")
	f.neighbourAgent = newAgent(f.neighbourID, f.neighbourRuntime, "neighbour agent")
	f.neighbourIssue = newIssue(f.neighbourID, "neighbour issue")

	f.taskViaAgent = insertDeletePathTask(t, f.victimAgent, f.neighbourIssue, f.neighbourRuntime)
	f.taskViaIssue = insertDeletePathTask(t, f.neighbourAgent, f.victimIssue, f.neighbourRuntime)
	f.taskViaRuntime = insertDeletePathTask(t, f.neighbourAgent, f.neighbourIssue, f.victimRuntime)
	f.neighbourTask = insertDeletePathTask(t, f.neighbourAgent, f.neighbourIssue, f.neighbourRuntime)

	f.victimToken = insertDeletePathToken(t, "mul5999-victim-"+slugSuffix, f.taskViaAgent, f.victimAgent, f.victimID)
	f.crossTaskToken = insertDeletePathToken(t, "mul5999-cross-task-"+slugSuffix, f.taskViaIssue, f.neighbourAgent, f.neighbourID)
	f.crossAgentToken = insertDeletePathToken(t, "mul5999-cross-agent-"+slugSuffix, f.neighbourTask, f.victimAgent, f.neighbourID)
	f.neighbourOnlyToken = insertDeletePathToken(t, "mul5999-neighbour-"+slugSuffix, f.neighbourTask, f.neighbourAgent, f.neighbourID)

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = testPool.Exec(bg, `DELETE FROM task_token WHERE id = ANY($1::uuid[])`,
			[]string{f.victimToken, f.crossTaskToken, f.crossAgentToken, f.neighbourOnlyToken})
		_, _ = testPool.Exec(bg, `DELETE FROM agent_task_queue WHERE id = ANY($1::uuid[])`,
			[]string{f.taskViaAgent, f.taskViaIssue, f.taskViaRuntime, f.neighbourTask})
	})

	return f
}

func insertDeletePathTask(t *testing.T, agentID, issueID, runtimeID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at)
VALUES ($1, $2, $3, 'completed', now())
RETURNING id
`, agentID, issueID, runtimeID).Scan(&id); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return id
}

func insertDeletePathToken(t *testing.T, hash, taskID, agentID, wsID string) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), `
INSERT INTO task_token (token_hash, task_id, agent_id, workspace_id, user_id, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + interval '1 hour')
RETURNING id
`, hash, taskID, agentID, wsID, testUserID).Scan(&id); err != nil {
		t.Fatalf("create task token %s: %v", hash, err)
	}
	return id
}

func rowExists(t *testing.T, table, id string) bool {
	t.Helper()
	var found bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&found); err != nil {
		t.Fatalf("check %s %s: %v", table, id, err)
	}
	return found
}

// TestDeleteWorkspaceTasks_IsSelfSufficientWithoutCascades asserts that the task
// sweep removes the rows on its own, inside the transaction and before any owner
// row is deleted. That ordering is the whole point: the legacy FK cascades are
// documented as an expand-phase safety net, so an assertion taken after the
// handler has finished cannot tell teardown apart from a cascade doing the work.
func TestDeleteWorkspaceTasks_IsSelfSufficientWithoutCascades(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "selfsufficient")

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	qtx := testHandler.Queries.WithTx(tx)

	if err := qtx.SetWorkspaceTeardownMode(ctx); err != nil {
		t.Fatalf("set teardown mode: %v", err)
	}
	owners, err := lockWorkspaceTaskOwners(ctx, qtx, parseUUID(f.victimID))
	if err != nil {
		t.Fatalf("lock task owners: %v", err)
	}
	if len(owners.Agents) != 1 || len(owners.Issues) != 1 || len(owners.Runtimes) != 1 {
		t.Fatalf("locked owners = %d agents / %d issues / %d runtimes, want 1 each",
			len(owners.Agents), len(owners.Issues), len(owners.Runtimes))
	}
	if err := deleteWorkspaceTasks(ctx, qtx, owners); err != nil {
		t.Fatalf("deleteWorkspaceTasks: %v", err)
	}

	// Still inside the transaction: no agent, issue or runtime row has been
	// deleted, so nothing here can be attributed to ON DELETE CASCADE.
	count := func(query string, args ...any) int {
		var n int
		if err := tx.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM agent_task_queue WHERE id = ANY($1::uuid[])`,
		[]string{f.taskViaAgent, f.taskViaIssue, f.taskViaRuntime}); n != 0 {
		t.Errorf("%d of the 3 ownership-path tasks survived the sweep", n)
	}
	if n := count(`SELECT count(*) FROM task_token WHERE id = ANY($1::uuid[])`,
		[]string{f.victimToken, f.crossTaskToken, f.crossAgentToken}); n != 0 {
		t.Errorf("%d of the 3 task_token paths survived the sweep", n)
	}
	if n := count(`SELECT count(*) FROM agent WHERE id = $1`, f.victimAgent); n != 1 {
		t.Fatalf("victim agent row was deleted by the task sweep; the assertions above prove nothing")
	}
	if n := count(`SELECT count(*) FROM agent_task_queue WHERE id = $1`, f.neighbourTask); n != 1 {
		t.Error("neighbour-only task was deleted by the victim's task sweep")
	}
	if n := count(`SELECT count(*) FROM task_token WHERE id = $1`, f.neighbourOnlyToken); n != 1 {
		t.Error("neighbour-only task token was deleted by the victim's task sweep")
	}
}

// TestDeleteWorkspace_CollectsTasksThroughEveryOwnershipPath is the end-to-end
// counterpart: the handler returns 204, every ownership path is gone, and the
// neighbour workspace is untouched.
func TestDeleteWorkspace_CollectsTasksThroughEveryOwnershipPath(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	f := newWorkspaceDeletePathFixture(t, "endtoend")

	w := httptest.NewRecorder()
	req := newRequest("DELETE", "/api/workspaces/"+f.victimID, nil)
	req = withURLParam(req, "id", f.victimID)
	testHandler.DeleteWorkspace(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("DeleteWorkspace = %d, want 204: %s", w.Code, w.Body.String())
	}

	for name, id := range map[string]string{
		"task reachable only via agent_id":   f.taskViaAgent,
		"task reachable only via issue_id":   f.taskViaIssue,
		"task reachable only via runtime_id": f.taskViaRuntime,
	} {
		if rowExists(t, "agent_task_queue", id) {
			t.Errorf("%s survived workspace delete", name)
		}
	}
	for name, id := range map[string]string{
		"token owned by the deleted workspace": f.victimToken,
		"token whose task is the victim's":     f.crossTaskToken,
		"token whose agent is the victim's":    f.crossAgentToken,
	} {
		if rowExists(t, "task_token", id) {
			t.Errorf("%s survived workspace delete", name)
		}
	}

	if !rowExists(t, "agent_task_queue", f.neighbourTask) {
		t.Error("neighbour workspace task was deleted")
	}
	if !rowExists(t, "task_token", f.neighbourOnlyToken) {
		t.Error("neighbour workspace task token was deleted")
	}
	if !rowExists(t, "agent", f.neighbourAgent) ||
		!rowExists(t, "issue", f.neighbourIssue) ||
		!rowExists(t, "agent_runtime", f.neighbourRuntime) {
		t.Error("neighbour workspace objects were deleted")
	}
	if rowExists(t, "workspace", f.victimID) {
		t.Error("victim workspace still exists")
	}
}

// TestDeleteWorkspaceTasks_PagesPastTheBatchSize covers the bound itself: a
// single owner with more tasks than one batch must be swept by several
// iterations, and the loop must terminate. Nothing here may depend on the whole
// task set fitting in one statement or in this process (MUL-5999 review).
func TestDeleteWorkspaceTasks_PagesPastTheBatchSize(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "batched")

	// Two and a half batches on one agent, so the loop has to page and then
	// see an empty page.
	const extraTasks = workspaceDeleteTaskBatchSize*2 + workspaceDeleteTaskBatchSize/2
	if _, err := testPool.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at)
SELECT $1, $2, $3, 'completed', now() FROM generate_series(1, $4::int)
`, f.victimAgent, f.victimIssue, f.victimRuntime, extraTasks); err != nil {
		t.Fatalf("seed %d tasks: %v", extraTasks, err)
	}

	tx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	qtx := testHandler.Queries.WithTx(tx)

	if err := qtx.SetWorkspaceTeardownMode(ctx); err != nil {
		t.Fatalf("set teardown mode: %v", err)
	}
	owners, err := lockWorkspaceTaskOwners(ctx, qtx, parseUUID(f.victimID))
	if err != nil {
		t.Fatalf("lock task owners: %v", err)
	}

	// The bound is the LIMIT on the page query. Without it the sweep would pull
	// a whole owner's task history into this process in one go, which is the
	// unbounded-memory failure mode this loop exists to avoid.
	for _, tc := range []struct {
		name string
		page func(int32) ([]pgtype.UUID, error)
	}{
		{"by agent", func(limit int32) ([]pgtype.UUID, error) {
			return qtx.ListTaskIDsByAgentBatch(ctx, db.ListTaskIDsByAgentBatchParams{AgentID: owners.Agents[0], Limit: limit})
		}},
		{"by issue", func(limit int32) ([]pgtype.UUID, error) {
			return qtx.ListTaskIDsByIssueBatch(ctx, db.ListTaskIDsByIssueBatchParams{IssueID: owners.Issues[0], Limit: limit})
		}},
		{"by runtime", func(limit int32) ([]pgtype.UUID, error) {
			return qtx.ListTaskIDsByRuntimeBatch(ctx, db.ListTaskIDsByRuntimeBatchParams{RuntimeID: owners.Runtimes[0], Limit: limit})
		}},
	} {
		const probeLimit = 10
		ids, err := tc.page(probeLimit)
		if err != nil {
			t.Fatalf("page %s: %v", tc.name, err)
		}
		if len(ids) != probeLimit {
			t.Errorf("page %s returned %d ids for LIMIT %d; the page query is not bounded",
				tc.name, len(ids), probeLimit)
		}
	}

	if err := deleteWorkspaceTasks(ctx, qtx, owners); err != nil {
		t.Fatalf("deleteWorkspaceTasks: %v", err)
	}

	var remaining int
	if err := tx.QueryRow(ctx, `
SELECT count(*) FROM agent_task_queue
WHERE agent_id = $1 OR issue_id = $2 OR runtime_id = $3
`, f.victimAgent, f.victimIssue, f.victimRuntime).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d tasks survived a multi-batch sweep", remaining)
	}
}

// TestDeleteWorkspaceTasks_FencesConcurrentEnqueueAndReassignment covers the two
// races a per-batch sweep would otherwise lose:
//
//   - enqueue after the sweep: a task inserted for an already-swept owner would
//     be left behind with its own leaf rows;
//   - reassignment during the sweep: a task moved to another workspace's runtime
//     between read and delete would be deleted on a stale ownership claim.
//
// Both are closed by locks the sweep takes, so the assertion is that a concurrent
// writer is blocked (and hits its own lock_timeout) while teardown holds them.
func TestDeleteWorkspaceTasks_FencesConcurrentEnqueueAndReassignment(t *testing.T) {
	if testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	f := newWorkspaceDeletePathFixture(t, "fence")

	tearDownTx, err := testHandler.TxStarter.Begin(ctx)
	if err != nil {
		t.Fatalf("begin teardown tx: %v", err)
	}
	defer tearDownTx.Rollback(ctx)
	qtx := testHandler.Queries.WithTx(tearDownTx)
	if _, err := lockWorkspaceTaskOwners(ctx, qtx, parseUUID(f.victimID)); err != nil {
		t.Fatalf("lock task owners: %v", err)
	}

	// A second transaction standing in for a concurrent writer. Its own short
	// lock_timeout turns "blocked forever" into an assertable error.
	blocked := func(name, sql string, args ...any) {
		t.Helper()
		other, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", name, err)
		}
		defer other.Rollback(ctx)
		if _, err := other.Exec(ctx, "SET LOCAL lock_timeout = 750"); err != nil {
			t.Fatalf("%s: set lock_timeout: %v", name, err)
		}
		_, err = other.Exec(ctx, sql, args...)
		if err == nil {
			t.Errorf("%s: succeeded while teardown held the owner locks; the fence is not working", name)
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Errorf("%s: got %v, want lock_not_available (55P03)", name, err)
		}
	}

	blocked("enqueue for a locked agent", `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at)
VALUES ($1, $2, $3, 'completed', now())
`, f.victimAgent, f.neighbourIssue, f.neighbourRuntime)

	blocked("enqueue against a locked runtime", `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at)
VALUES ($1, $2, $3, 'completed', now())
`, f.neighbourAgent, f.neighbourIssue, f.victimRuntime)

	blocked("reassignment onto a locked runtime", `
UPDATE agent_task_queue SET runtime_id = $1 WHERE id = $2
`, f.victimRuntime, f.neighbourTask)

	// A writer that has nothing to do with the workspace under teardown must
	// still get through, so the fence is not a global stop-the-world.
	unrelated, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin unrelated tx: %v", err)
	}
	defer unrelated.Rollback(ctx)
	if _, err := unrelated.Exec(ctx, "SET LOCAL lock_timeout = 750"); err != nil {
		t.Fatalf("unrelated: set lock_timeout: %v", err)
	}
	if _, err := unrelated.Exec(ctx, `
INSERT INTO agent_task_queue (agent_id, issue_id, runtime_id, status, completed_at)
VALUES ($1, $2, $3, 'completed', now())
`, f.neighbourAgent, f.neighbourIssue, f.neighbourRuntime); err != nil {
		t.Errorf("neighbour-only enqueue was blocked by the victim's teardown locks: %v", err)
	}
}
