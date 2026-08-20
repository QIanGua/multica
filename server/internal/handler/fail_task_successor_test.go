package handler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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

// TestCreateRetryTask_YieldsPendingSlotInsteadOfRaising is the deterministic half
// of the concurrency contract. The successor pre-check in FailTask takes no lock
// (a plain count under READ COMMITTED), so a rerun can always commit between that
// check and the retry insert. What makes the failure transaction safe regardless
// of interleaving is the insert itself: ON CONFLICT DO NOTHING yields the slot
// rather than raising 23505 and aborting the caller's transaction.
//
// Asserting pgx.ErrNoRows here — not a unique violation — is what proves the
// enclosing transaction can never be poisoned by losing the race.
func TestCreateRetryTask_YieldsPendingSlotInsteadOfRaising(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID := dbfx.Runtime(t, "retry-yield-runtime")
	agentID := dbfx.Agent(t, "retry-yield-agent", runtimeID)
	issueID := dbfx.Issue(t, "retry yields the pending slot", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	// A failed parent that is otherwise a perfectly good retry candidate.
	parentID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":       issueID,
		"runtime_id":     runtimeID,
		"status":         "failed",
		"failure_reason": "timeout",
		"attempt":        1,
		"max_attempts":   2,
	})
	// Someone else already holds the single queued/dispatched slot.
	dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "queued",
	})

	_, err := testHandler.TaskService.Queries.CreateRetryTask(ctx, db.CreateRetryTaskParams{ID: parseUUID(parentID)})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateRetryTask against an occupied slot must yield (pgx.ErrNoRows), got %v", err)
	}
}

// TestFailTaskAndRerunConcurrently_NeverStrandsRunningTask forces the interleaving
// the successor pre-check cannot prevent: FailTask and RerunIssue racing for the
// same (issue, agent) slot on two separate connections, released together from a
// barrier so the rerun can commit inside FailTask's transaction window.
//
// The invariants must hold no matter who wins:
//   - FailTask never returns an error (its transaction is never aborted by the
//     retry insert), so the parent's failure always commits.
//   - The parent ends 'failed', never stranded back in 'running'.
//   - At most one runnable successor exists, since the unique index allows one.
func TestFailTaskAndRerunConcurrently_NeverStrandsRunningTask(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID := dbfx.Runtime(t, "rerun-race-runtime")
	agentID := dbfx.Agent(t, "rerun-race-agent", runtimeID)
	issueID := dbfx.Issue(t, "rerun races auto-retry", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	const rounds = 15
	for i := 0; i < rounds; i++ {
		runningID := dbfx.Task(t, agentID, testutil.Cols{
			"issue_id":     issueID,
			"runtime_id":   runtimeID,
			"status":       "running",
			"attempt":      1,
			"max_attempts": 2,
		})

		start := make(chan struct{})
		var wg sync.WaitGroup
		var failErr, rerunErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, failErr = testHandler.TaskService.FailTask(ctx, parseUUID(runningID),
				"run died", "", "", "", "timeout", false, "", "")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, rerunErr = testHandler.TaskService.RerunIssue(ctx, parseUUID(issueID), pgtype.UUID{}, pgtype.UUID{}, parseUUID(testUserID), nil)
		}()
		close(start)
		wg.Wait()

		if failErr != nil {
			t.Fatalf("round %d: FailTask must never abort on slot contention: %v", i, failErr)
		}
		if rerunErr != nil {
			t.Fatalf("round %d: RerunIssue must reclaim the slot rather than surface a conflict: %v", i, rerunErr)
		}

		var status string
		if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, runningID).Scan(&status); err != nil {
			t.Fatalf("round %d: read parent: %v", i, err)
		}
		if status != "failed" {
			t.Fatalf("round %d: parent must commit as failed, got %q (stranded)", i, status)
		}

		var pending int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM agent_task_queue
			WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')
		`, issueID, agentID).Scan(&pending); err != nil {
			t.Fatalf("round %d: count successors: %v", i, err)
		}
		if pending > 1 {
			t.Fatalf("round %d: the unique index allows one runnable successor, found %d", i, pending)
		}

		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	}
}

// TestPromoteDueDeferred_SkipsWhenPendingSlotOccupied covers the deferred half of
// the slot contention. runtime_offline and provider_network's final attempt arm
// their retry as a 'deferred' row, which is NOT covered by
// idx_one_pending_task_per_issue_agent_v2 — so it can be created alongside a
// manual rerun (rerun clears the slot, the retry commits, the rerun's enqueue
// then succeeds because deferred does not conflict).
//
// Promotion is where that pair becomes a problem: flipping the deferred row to
// 'queued' collides with the rerun already holding the slot, and since the claim
// loop promotes before it claims, the resulting error blocked every claim on the
// runtime — starving the very rerun the operator asked for.
func TestPromoteDueDeferred_SkipsWhenPendingSlotOccupied(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID := dbfx.Runtime(t, "promote-fence-runtime")
	agentID := dbfx.Agent(t, "promote-fence-agent", runtimeID)
	issueID := dbfx.Issue(t, "deferred retry meets manual rerun", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	// The manual rerun holds the single queued slot.
	rerunID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "queued",
	})
	// The deferred retry armed by the old task's runtime_offline failure, already due.
	deferredID := dbfx.Task(t, agentID, testutil.Cols{
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "deferred",
		"fire_at":    testutil.Raw("now() - interval '1 minute'"),
	})

	promoted, err := testHandler.TaskService.Queries.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{
		RuntimeID:        parseUUID(runtimeID),
		RuntimeStaleSecs: 300,
	})
	if err != nil {
		t.Fatalf("promotion must not fail while the slot is occupied (it would block every claim on this runtime): %v", err)
	}
	for _, p := range promoted {
		if uuidToString(p.ID) == deferredID {
			t.Fatal("the deferred retry must not be promoted into an occupied slot")
		}
	}
	if got := taskStatusByID(t, deferredID); got != "deferred" {
		t.Fatalf("deferred retry should stay deferred until the slot frees, got %q", got)
	}
	if got := taskStatusByID(t, rerunID); got != "queued" {
		t.Fatalf("the manual rerun must be untouched, got %q", got)
	}

	// Once the rerun leaves the slot, the retry is free to promote — nothing was lost.
	if _, err := testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed' WHERE id = $1`, rerunID); err != nil {
		t.Fatalf("free the slot: %v", err)
	}
	if _, err := testHandler.TaskService.Queries.PromoteDueDeferredTasksForRuntime(ctx, db.PromoteDueDeferredTasksForRuntimeParams{
		RuntimeID:        parseUUID(runtimeID),
		RuntimeStaleSecs: 300,
	}); err != nil {
		t.Fatalf("second promotion: %v", err)
	}
	if got := taskStatusByID(t, deferredID); got != "queued" {
		t.Fatalf("deferred retry should promote once the slot is free, got %q", got)
	}
}

func taskStatusByID(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT status FROM agent_task_queue WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read task status: %v", err)
	}
	return status
}

// TestFailTaskAndRerunConcurrently_NonAssigneeTarget runs the same race as
// TestFailTaskAndRerunConcurrently_NeverStrandsRunningTask, but for a rerun whose
// target is NOT the issue's current agent assignee — a past task re-fired by
// task_id after the issue was reassigned.
//
// That routes the enqueue through enqueueMentionTaskWithCommentPlan, which
// normalizes the unique violation into the bare ErrDuplicatePendingTask sentinel
// instead of surfacing the pgconn error. The reclaim branch has to recognise that
// shape too, otherwise every squad-leader / displaced-agent / mentioned-agent
// rerun losing this race reports a hard failure to the operator.
func TestFailTaskAndRerunConcurrently_NonAssigneeTarget(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	runtimeID := dbfx.Runtime(t, "non-assignee-race-runtime")
	assigneeID := dbfx.Agent(t, "non-assignee-race-current", runtimeID)
	displacedID := dbfx.Agent(t, "non-assignee-race-displaced", runtimeID)
	issueID := dbfx.Issue(t, "rerun a displaced agent while it fails", testutil.Cols{
		"assignee_type": "agent",
		"assignee_id":   assigneeID,
	})
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	})

	// The execution-log row the operator clicks retry on: it belonged to the
	// displaced agent, so the rerun targets that agent rather than the assignee.
	sourceTaskID := dbfx.Task(t, displacedID, testutil.Cols{
		"issue_id":   issueID,
		"runtime_id": runtimeID,
		"status":     "completed",
	})

	const rounds = 15
	for i := 0; i < rounds; i++ {
		runningID := dbfx.Task(t, displacedID, testutil.Cols{
			"issue_id":     issueID,
			"runtime_id":   runtimeID,
			"status":       "running",
			"attempt":      1,
			"max_attempts": 2,
		})

		start := make(chan struct{})
		var wg sync.WaitGroup
		var failErr, rerunErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, failErr = testHandler.TaskService.FailTask(ctx, parseUUID(runningID),
				"run died", "", "", "", "timeout", false, "", "")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, rerunErr = testHandler.TaskService.RerunIssue(ctx, parseUUID(issueID), parseUUID(sourceTaskID), pgtype.UUID{}, parseUUID(testUserID), nil)
		}()
		close(start)
		wg.Wait()

		if failErr != nil {
			t.Fatalf("round %d: FailTask must never abort on slot contention: %v", i, failErr)
		}
		if rerunErr != nil {
			t.Fatalf("round %d: a non-assignee rerun must reclaim the slot, not surface the normalized sentinel: %v", i, rerunErr)
		}
		if got := taskStatusByID(t, runningID); got != "failed" {
			t.Fatalf("round %d: parent must commit as failed, got %q", i, got)
		}

		var pending int
		if err := testPool.QueryRow(ctx, `
			SELECT count(*) FROM agent_task_queue
			WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched')
		`, issueID, displacedID).Scan(&pending); err != nil {
			t.Fatalf("round %d: count successors: %v", i, err)
		}
		if pending > 1 {
			t.Fatalf("round %d: expected at most one runnable successor, found %d", i, pending)
		}

		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1 AND id <> $2`, issueID, sourceTaskID)
	}
}
