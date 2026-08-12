package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func localDirRef(t *testing.T, path, daemonID, mode string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"local_path": path, "daemon_id": daemonID, "execution_mode": mode,
	})
	if err != nil {
		t.Fatalf("marshal ref: %v", err)
	}
	return raw
}

func runtimeWithVersion(daemonID, cliVersion string) db.AgentRuntime {
	rt := db.AgentRuntime{DaemonID: pgtype.Text{String: daemonID, Valid: daemonID != ""}}
	if cliVersion != "" {
		rt.Metadata, _ = json.Marshal(map[string]string{"cli_version": cliVersion})
	}
	return rt
}

// The save-time gate cannot cover a machine downgraded after the resource was
// written, and an old daemon json-skips execution_mode entirely — it would run
// the task IN PLACE, editing the working copy the user asked to isolate. This
// is the last point where that can be stopped.
func TestWorktreeClaimBlockReason(t *testing.T) {
	const daemon = "daemon-a"

	worktreeRes := []ProjectResourceData{{
		ID: "r1", ResourceType: "local_directory",
		ResourceRef: localDirRef(t, "/Users/dev/game", daemon, "worktree"),
	}}

	t.Run("blocks a runtime below the floor", func(t *testing.T) {
		reason := worktreeClaimBlockReason(worktreeRes, runtimeWithVersion(daemon, "0.4.10"))
		if reason == "" {
			t.Fatal("an outdated runtime was allowed to claim a worktree task")
		}
		if !strings.Contains(reason, "/Users/dev/game") || !strings.Contains(reason, "0.4.10") {
			t.Errorf("reason should name the directory and the version, got: %q", reason)
		}
	})

	t.Run("blocks a runtime reporting no version at all", func(t *testing.T) {
		// Fail closed: "no version" is what a daemon far older than the field
		// looks like, which is exactly the dangerous case.
		if worktreeClaimBlockReason(worktreeRes, runtimeWithVersion(daemon, "")) == "" {
			t.Error("a runtime with no reported version was allowed to claim")
		}
	})

	t.Run("allows a runtime at or above the floor", func(t *testing.T) {
		if reason := worktreeClaimBlockReason(worktreeRes, runtimeWithVersion(daemon, "9.9.9")); reason != "" {
			t.Errorf("a new enough runtime was blocked: %q", reason)
		}
	})

	t.Run("ignores in_place and absent modes", func(t *testing.T) {
		for _, mode := range []string{"in_place", ""} {
			res := []ProjectResourceData{{
				ID: "r1", ResourceType: "local_directory",
				ResourceRef: localDirRef(t, "/Users/dev/game", daemon, mode),
			}}
			if reason := worktreeClaimBlockReason(res, runtimeWithVersion(daemon, "0.1.0")); reason != "" {
				t.Errorf("mode %q blocked an old daemon that can run it fine: %q", mode, reason)
			}
		}
	})

	// A project may carry one local_directory per machine. Another machine's
	// worktree resource says nothing about this runtime's ability to run.
	t.Run("ignores a resource bound to a different daemon", func(t *testing.T) {
		other := []ProjectResourceData{{
			ID: "r1", ResourceType: "local_directory",
			ResourceRef: localDirRef(t, "/Users/dev/game", "daemon-b", "worktree"),
		}}
		if reason := worktreeClaimBlockReason(other, runtimeWithVersion(daemon, "0.1.0")); reason != "" {
			t.Errorf("another machine's resource blocked this claim: %q", reason)
		}
	})

	t.Run("ignores non-local_directory resources", func(t *testing.T) {
		repo := []ProjectResourceData{{
			ID: "r1", ResourceType: "github_repo",
			ResourceRef: json.RawMessage(`{"url":"https://github.com/a/b"}`),
		}}
		if reason := worktreeClaimBlockReason(repo, runtimeWithVersion(daemon, "0.1.0")); reason != "" {
			t.Errorf("github_repo resource blocked a claim: %q", reason)
		}
	})

	t.Run("ignores a runtime with no daemon id", func(t *testing.T) {
		if reason := worktreeClaimBlockReason(worktreeRes, runtimeWithVersion("", "0.1.0")); reason != "" {
			t.Errorf("cloud runtime blocked: %q", reason)
		}
	})

	t.Run("survives a malformed ref", func(t *testing.T) {
		bad := []ProjectResourceData{{
			ID: "r1", ResourceType: "local_directory",
			ResourceRef: json.RawMessage(`{"local_path": 42}`),
		}}
		if reason := worktreeClaimBlockReason(bad, runtimeWithVersion(daemon, "0.1.0")); reason != "" {
			t.Errorf("malformed ref produced a block: %q", reason)
		}
	})
}

// branch_name has to survive the whole way to the task row on BOTH terminal
// paths. It was previously dropped at decode (the request struct had no field),
// so the daemon sent it and nobody received it. The failure path matters most:
// worktree mode commits the agent's leftovers before tearing the worktree down,
// so a run that died partway still produced something the user needs to find.
func TestBranchNameRoundTripsThroughBothTerminalPaths(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}

	// The terminal handlers resolve the task's workspace through its issue, so
	// a bare task row reads as "failed to load task".
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'branch name round-trip fixture', 'in_progress', 'none', $2, 'member', 993310, 0)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("setup: create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	newRunningTask := func(t *testing.T) string {
		t.Helper()
		var taskID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
			VALUES ($1, $2, $3, 'running', 0, now())
			RETURNING id
		`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
			t.Fatalf("setup: create task: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
		return taskID
	}

	post := func(t *testing.T, taskID, endpoint string, body map[string]any, call func(http.ResponseWriter, *http.Request)) {
		t.Helper()
		w := httptest.NewRecorder()
		req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/"+endpoint,
			body, testWorkspaceID, "legit-daemon")
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskId", taskID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		call(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", endpoint, w.Code, w.Body.String())
		}
	}

	readBranch := func(t *testing.T, taskID string) string {
		t.Helper()
		var branch pgtype.Text
		if err := testPool.QueryRow(ctx,
			`SELECT branch_name FROM agent_task_queue WHERE id = $1`, taskID).Scan(&branch); err != nil {
			t.Fatalf("read branch_name: %v", err)
		}
		return branch.String
	}

	t.Run("complete", func(t *testing.T) {
		taskID := newRunningTask(t)
		post(t, taskID, "complete", map[string]any{
			"output": "done", "branch_name": "agent/j/abc12345",
		}, testHandler.CompleteTask)
		if got := readBranch(t, taskID); got != "agent/j/abc12345" {
			t.Errorf("branch_name = %q, want it persisted from the complete callback", got)
		}
	})

	t.Run("fail", func(t *testing.T) {
		taskID := newRunningTask(t)
		post(t, taskID, "fail", map[string]any{
			"error": "agent crashed", "branch_name": "agent/j/def67890",
		}, testHandler.FailTask)
		if got := readBranch(t, taskID); got != "agent/j/def67890" {
			t.Errorf("branch_name = %q, want the partial work still findable after a failure", got)
		}
	})

	t.Run("absent stays empty for non-worktree tasks", func(t *testing.T) {
		taskID := newRunningTask(t)
		post(t, taskID, "complete", map[string]any{"output": "done"}, testHandler.CompleteTask)
		if got := readBranch(t, taskID); got != "" {
			t.Errorf("branch_name = %q, want empty when the daemon reports none", got)
		}
	})
}

// Every terminal path that can carry a branch must carry it. A worktree run
// commits before it learns its fate, so the branch is equally real whether the
// run completed, failed, was rerouted, or was cancelled — and the user has no
// other way to find it.
func TestBranchNameSurvivesEveryTerminalPath(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}
	var issueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES ($1, 'terminal path branch fixture', 'in_progress', 'none', $2, 'member', 993311, 0)
		RETURNING id
	`, testWorkspaceID, testUserID).Scan(&issueID); err != nil {
		t.Fatalf("setup: create issue: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	newTask := func(t *testing.T, status string) string {
		t.Helper()
		var taskID string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
			VALUES ($1, $2, $3, $4, 0, now())
			RETURNING id
		`, agentID, runtimeID, issueID, status).Scan(&taskID); err != nil {
			t.Fatalf("setup: create task: %v", err)
		}
		t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })
		return taskID
	}
	post := func(t *testing.T, taskID, endpoint string, body map[string]any, call func(http.ResponseWriter, *http.Request)) {
		t.Helper()
		w := httptest.NewRecorder()
		req := newDaemonTokenRequest("POST", "/api/daemon/tasks/"+taskID+"/"+endpoint,
			body, testWorkspaceID, "legit-daemon")
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("taskId", taskID)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		call(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", endpoint, w.Code, w.Body.String())
		}
	}
	readBranch := func(t *testing.T, taskID string) string {
		t.Helper()
		var branch pgtype.Text
		if err := testPool.QueryRow(ctx,
			`SELECT branch_name FROM agent_task_queue WHERE id = $1`, taskID).Scan(&branch); err != nil {
			t.Fatalf("read branch_name: %v", err)
		}
		return branch.String
	}

	// A context-exhausted "success" is rerouted to the failure path server-side.
	// The run still delivered a branch; the reroute must not eat it.
	t.Run("context-exhausted reroute keeps the branch", func(t *testing.T) {
		taskID := newTask(t, "running")
		post(t, taskID, "complete", map[string]any{
			"output":      "Prompt is too long and cannot be compacted further.",
			"branch_name": "agent/j/ctx11111",
		}, testHandler.CompleteTask)

		var status string
		if err := testPool.QueryRow(ctx,
			`SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "failed" {
			t.Fatalf("precondition: expected the reroute to record a failure, got %q", status)
		}
		if got := readBranch(t, taskID); got != "agent/j/ctx11111" {
			t.Errorf("branch_name = %q, want it carried across the complete→fail reroute", got)
		}
	})

	// Cancellation discards the whole result payload, but the worktree was
	// finalized before the daemon learned of the cancel — so the branch exists
	// and the ack is the only channel left to report it.
	t.Run("cancel ack records the branch", func(t *testing.T) {
		taskID := newTask(t, "cancelled")
		post(t, taskID, "cancel-ack", map[string]any{
			"branch_name": "agent/j/cancel11",
		}, testHandler.AckTaskCancelled)
		if got := readBranch(t, taskID); got != "agent/j/cancel11" {
			t.Errorf("branch_name = %q, want the cancelled run's committed work discoverable", got)
		}
	})

	// Older daemons post an empty body here; that must stay a valid ack.
	t.Run("cancel ack without a branch still succeeds", func(t *testing.T) {
		taskID := newTask(t, "cancelled")
		post(t, taskID, "cancel-ack", map[string]any{}, testHandler.AckTaskCancelled)
		if got := readBranch(t, taskID); got != "" {
			t.Errorf("branch_name = %q, want empty", got)
		}
	})

	// A name recorded by a terminal callback is authoritative; a later ack must
	// not clobber it.
	t.Run("cancel ack does not overwrite an existing branch", func(t *testing.T) {
		taskID := newTask(t, "running")
		post(t, taskID, "complete", map[string]any{
			"output": "done", "branch_name": "agent/j/original",
		}, testHandler.CompleteTask)
		post(t, taskID, "cancel-ack", map[string]any{
			"branch_name": "agent/j/latecomer",
		}, testHandler.AckTaskCancelled)
		if got := readBranch(t, taskID); got != "agent/j/original" {
			t.Errorf("branch_name = %q, want the original preserved", got)
		}
	})
}

// The claim gate cancels the task; the reason has to land on the row. The 4xx
// only reaches the daemon's log, and batch claim drops the task from its
// response entirely — so without a persisted reason the user sees a bare
// "cancelled" for a condition only they can fix.
func TestWorktreeClaimGateCancelPersistsReason(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	var agentID, runtimeID string
	if err := testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID); err != nil {
		t.Fatalf("setup: get agent: %v", err)
	}
	var taskID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority, dispatched_at)
		VALUES ($1, $2, 'dispatched', 0, now())
		RETURNING id
	`, agentID, runtimeID).Scan(&taskID); err != nil {
		t.Fatalf("setup: create task: %v", err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	const reason = "worktree mode needs a newer runtime on that machine"
	if _, err := testHandler.Queries.CancelAgentTaskWithReason(ctx, db.CancelAgentTaskWithReasonParams{
		ID:            parseUUID(taskID),
		Error:         pgtype.Text{String: reason, Valid: true},
		FailureReason: pgtype.Text{String: "local_directory_error", Valid: true},
	}); err != nil {
		t.Fatalf("CancelAgentTaskWithReason: %v", err)
	}

	var status string
	var errText, failureReason pgtype.Text
	if err := testPool.QueryRow(ctx,
		`SELECT status, error, failure_reason FROM agent_task_queue WHERE id = $1`,
		taskID).Scan(&status, &errText, &failureReason); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("status = %q, want cancelled", status)
	}
	if errText.String != reason {
		t.Errorf("error = %q, want the user-facing reason persisted", errText.String)
	}
	if failureReason.String != "local_directory_error" {
		t.Errorf("failure_reason = %q, want local_directory_error", failureReason.String)
	}
}
