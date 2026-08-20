package handler

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
)

// TestFailTask_SkipsAutoRetryWhenManualRerunAlreadyQueued covers the interaction
// opened up by letting a manual rerun queue BEHIND a running task instead of
// cancelling it (MUL-6146).
//
// Sequence: a task is running, an operator reruns the issue (so a queued row now
// sits behind it), and then the running task fails for a reason whose retry
// child is created immediately rather than deferred. That child is inserted as a
// second queued row for the same (issue, agent) and collides with
// idx_one_pending_task_per_issue_agent_v2. That insert shares FailTask's
// transaction, so the unique violation would roll the parent's failed status back
// with it: the fail call surfaces an error and the task is left stuck in
// 'running'.
//
// Expected instead: the failure commits, the auto-retry is skipped because a
// successor already exists, and exactly one runnable row remains — the rerun.
//
// The reason matters: retryDelayForAttempt defers runtime_offline and
// provider_network's FINAL attempt, and a deferred child is not covered by the
// unique index, so those cannot collide. The cases below are the ones that
// produce an immediately-claimable 'queued' child.
func TestFailTask_SkipsAutoRetryWhenManualRerunAlreadyQueued(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	for _, tc := range []struct {
		name          string
		failureReason string
	}{
		{name: "timeout", failureReason: "timeout"},
		{name: "first_provider_network", failureReason: "agent_error.provider_network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failTaskSuccessorCase(t, tc.failureReason)
		})
	}
}

func failTaskSuccessorCase(t *testing.T, failureReason string) {
	t.Helper()
	ctx := context.Background()

	runtimeID := dbfx.Runtime(t, "fail-successor-runtime-"+failureReason)
	agentID := dbfx.Agent(t, "fail-successor-agent-"+failureReason, runtimeID)
	issueID := dbfx.Issue(t, "manual rerun races auto-retry", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	// The in-flight run. attempt/max_attempts leave the auto-retry budget open,
	// so without the successor check FailTask would try to insert a retry child.
	runningID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":     issueID,
		"runtime_id":   runtimeID,
		"status":       "running",
		"attempt":      1,
		"max_attempts": 2,
	})

	// The operator reruns while that task is still running: allowed now, and it
	// takes the single queued/dispatched slot the unique index permits.
	rerun, err := testHandler.TaskService.RerunIssue(ctx, parseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, parseUUID(testUserID), nil)
	if err != nil {
		t.Fatalf("RerunIssue behind running task: %v", err)
	}
	if rerun.Status != "queued" {
		t.Fatalf("precondition: rerun should be queued, got %q", rerun.Status)
	}

	// The retry child for this reason is created immediately as 'queued' — the
	// exact shape that collides with the rerun already holding the slot.
	if _, err := testHandler.TaskService.FailTask(ctx, parseUUID(runningID),
		"run died", "", "", "", failureReason, false, "", ""); err != nil {
		t.Fatalf("FailTask with a manual rerun already queued: %v", err)
	}

	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, runningID).Scan(&status); err != nil {
		t.Fatalf("read failed task: %v", err)
	}
	if status != "failed" {
		t.Fatalf("the parent's failure must commit even when the retry is skipped; status = %q", status)
	}

	rows, err := testPool.Query(ctx, `
		SELECT id::text FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')
	`, issueID, agentID)
	if err != nil {
		t.Fatalf("list runnable successors: %v", err)
	}
	defer rows.Close()
	var successors []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan successor: %v", err)
		}
		successors = append(successors, id)
	}
	if len(successors) != 1 {
		t.Fatalf("expected exactly one runnable successor, got %d: %v", len(successors), successors)
	}
	if successors[0] != uuidToString(rerun.ID) {
		t.Fatalf("the surviving successor should be the manual rerun %s, got %s", uuidToString(rerun.ID), successors[0])
	}
}
