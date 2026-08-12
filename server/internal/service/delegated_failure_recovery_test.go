package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/attribution"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type delegatedFailureFixture struct {
	pool        *pgxpool.Pool
	workspaceID string
	userID      string
	issueID     string
	workerIssue string
	runtimeID   string
	coordinator string
	worker      string
	sourceTask  string
}

func seedDelegatedFailureFixture(t *testing.T) (*delegatedFailureFixture, *TaskService) {
	t.Helper()
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	workspaceID, userID, coordinatorID, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status = 'in_progress' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("activate source issue: %v", err)
	}

	var runtimeID string
	if err := pool.QueryRow(ctx, `SELECT runtime_id::text FROM agent WHERE id = $1`, coordinatorID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	var workerID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, runtime_mode, runtime_config, runtime_id, visibility,
			max_concurrent_tasks, owner_id, instructions, custom_env, custom_args
		)
		VALUES ($1, 'delegated-worker', 'cloud', '{}'::jsonb, $2, 'workspace', 1, $3, '', '{}'::jsonb, '[]'::jsonb)
		RETURNING id`, workspaceID, runtimeID, userID).Scan(&workerID); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	var workerIssueID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO issue (
			workspace_id, title, creator_type, creator_id, assignee_type, assignee_id,
			priority, parent_issue_id, number
		)
		VALUES ($1, 'delegated worker issue', 'member', $2, 'agent', $3, 'medium', $4, 2)
		RETURNING id`, workspaceID, userID, workerID, issueID).Scan(&workerIssueID); err != nil {
		t.Fatalf("seed worker issue: %v", err)
	}

	var sourceTaskID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority,
			originator_user_id, accountable_user_id, originator_source
		)
		VALUES ($1, $2, $3, 'completed', 0, $4, $4, 'direct_human')
		RETURNING id`, coordinatorID, runtimeID, issueID, userID).Scan(&sourceTaskID); err != nil {
		t.Fatalf("seed source task: %v", err)
	}

	return &delegatedFailureFixture{
		pool:        pool,
		workspaceID: workspaceID,
		userID:      userID,
		issueID:     issueID,
		workerIssue: workerIssueID,
		runtimeID:   runtimeID,
		coordinator: coordinatorID,
		worker:      workerID,
		sourceTask:  sourceTaskID,
	}, NewTaskService(db.New(pool), pool, nil, events.New())
}

func (f *delegatedFailureFixture) insertWorkerTask(t *testing.T, status, evidenceKind string, attempt, maxAttempts int32) pgtype.UUID {
	t.Helper()
	var taskID pgtype.UUID
	if err := f.pool.QueryRow(context.Background(), `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority, attempt, max_attempts,
			originator_user_id, accountable_user_id, originator_source,
			delegated_from_task_id, trigger_evidence_kind
		)
		VALUES ($1, $2, $3, $4, 0, $5, $6, $7, $7, 'delegation', $8, NULLIF($9, ''))
		RETURNING id`, f.worker, f.runtimeID, f.workerIssue, status, attempt, maxAttempts, f.userID, f.sourceTask, evidenceKind).Scan(&taskID); err != nil {
		t.Fatalf("seed worker task: %v", err)
	}
	return taskID
}

func TestFailTaskFinalDelegatedFailureWakesCoordinatorOnce(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 2)
	secret := "sk-" + strings.Repeat("a", 24)

	failed, err := svc.FailTask(ctx, failedID, "upstream capacity exhausted "+secret, "", "", "agent_error.process_failure", false, "")
	if err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var recoveryCount int
	var recoveryAgent, recoveryIssue, evidenceRef, delegatedFrom string
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(max(agent_id::text), ''), COALESCE(max(issue_id::text), ''),
		       COALESCE(max(trigger_evidence_ref_id::text), ''), COALESCE(max(delegated_from_task_id::text), '')
		FROM agent_task_queue
		WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).
		Scan(&recoveryCount, &recoveryAgent, &recoveryIssue, &evidenceRef, &delegatedFrom); err != nil {
		t.Fatalf("read recovery task: %v", err)
	}
	if recoveryCount != 1 {
		t.Fatalf("recovery task count = %d, want 1", recoveryCount)
	}
	if recoveryAgent != f.coordinator || recoveryIssue != f.issueID {
		t.Fatalf("recovery target = agent %s issue %s, want %s/%s", recoveryAgent, recoveryIssue, f.coordinator, f.issueID)
	}
	if evidenceRef != util.UUIDToString(failedID) || delegatedFrom != util.UUIDToString(failedID) {
		t.Fatalf("recovery lineage = evidence %s delegated_from %s, want failed task %s", evidenceRef, delegatedFrom, util.UUIDToString(failedID))
	}
	var retryCount int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, failedID).Scan(&retryCount); err != nil {
		t.Fatalf("count process-failure retries: %v", err)
	}
	if retryCount != 0 {
		t.Fatalf("process-failure retry count = %d, want 0", retryCount)
	}

	var commentCount int
	var content string
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(max(content), '') FROM comment
		WHERE issue_id = $1 AND author_type = 'system' AND type = 'progress_update' AND source_task_id = $2`, f.issueID, failedID).
		Scan(&commentCount, &content); err != nil {
		t.Fatalf("read recovery comment: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("recovery comment count = %d, want 1", commentCount)
	}
	if strings.Contains(content, secret) || !strings.Contains(content, "[REDACTED API KEY]") {
		t.Fatalf("recovery comment did not redact error: %q", content)
	}
	var failedIssueComments int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM comment
		WHERE issue_id = $1 AND type = 'system' AND source_task_id = $2`, f.workerIssue, failedID).Scan(&failedIssueComments); err != nil {
		t.Fatalf("count failed-issue comments: %v", err)
	}
	if failedIssueComments != 1 {
		t.Fatalf("failed-issue comments = %d, want legacy failure comment preserved", failedIssueComments)
	}

	// Replaying the sweeper/direct-failure side effect for the same terminal row
	// must not create another comment or another coordinator run.
	if handled, err := svc.recoverDelegatedTaskFailure(ctx, *failed); err != nil || !handled {
		t.Fatalf("repeat recovery = handled %v err %v, want handled without error", handled, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'progress_update' AND source_task_id = $2`, f.issueID, failedID).Scan(&commentCount); err != nil {
		t.Fatalf("recount recovery comments: %v", err)
	}
	if commentCount != 1 {
		t.Fatalf("replayed recovery comment count = %d, want 1", commentCount)
	}
	if _, err := f.pool.Exec(ctx, `
		UPDATE agent_task_queue
		SET status = 'failed', completed_at = now(), failure_reason = 'agent_error.process_failure'
		WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID); err != nil {
		t.Fatalf("fail recovery task: %v", err)
	}
	if handled, err := svc.recoverDelegatedTaskFailure(ctx, *failed); err != nil || !handled {
		t.Fatalf("recovery after terminal recovery task = handled %v err %v", handled, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).Scan(&recoveryCount); err != nil {
		t.Fatalf("recount recovery tasks: %v", err)
	}
	if recoveryCount != 1 {
		t.Fatalf("terminal recovery was recreated: task count = %d, want 1", recoveryCount)
	}
}

func TestHandleFailedTasksFinalDelegatedFailureWakesCoordinator(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "failed", "comment", 1, 2)
	if _, err := f.pool.Exec(ctx, `
		UPDATE agent_task_queue
		SET failure_reason = 'agent_error.process_failure', error = 'worker process exited', completed_at = now()
		WHERE id = $1`, failedID); err != nil {
		t.Fatalf("stamp failed task: %v", err)
	}
	failed, err := svc.Queries.GetAgentTask(ctx, failedID)
	if err != nil {
		t.Fatalf("load failed task: %v", err)
	}

	if retried := svc.HandleFailedTasks(ctx, []db.AgentTaskQueue{failed}); retried != 0 {
		t.Fatalf("HandleFailedTasks retried = %d, want 0", retried)
	}

	var recoveryTasks, recoveryComments int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).Scan(&recoveryTasks); err != nil {
		t.Fatalf("count recovery tasks: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE issue_id = $1 AND type = 'progress_update' AND source_task_id = $2`, f.issueID, failedID).Scan(&recoveryComments); err != nil {
		t.Fatalf("count recovery comments: %v", err)
	}
	if recoveryTasks != 1 || recoveryComments != 1 {
		t.Fatalf("sweeper recovery tasks/comments = %d/%d, want 1/1", recoveryTasks, recoveryComments)
	}
}

func TestFailTaskRetryPendingDoesNotWakeCoordinator(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 2)

	if _, err := svc.FailTask(ctx, failedID, "task timed out", "", "", "timeout", false, ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var retries, recoveries, comments int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id = $1`, failedID).Scan(&retries); err != nil {
		t.Fatalf("count retries: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).Scan(&recoveries); err != nil {
		t.Fatalf("count recoveries: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE type = 'progress_update' AND source_task_id = $1`, failedID).Scan(&comments); err != nil {
		t.Fatalf("count recovery comments: %v", err)
	}
	if retries != 1 || recoveries != 0 || comments != 0 {
		t.Fatalf("retry/recovery/comments = %d/%d/%d, want 1/0/0", retries, recoveries, comments)
	}
}

func TestFinalDelegatedFailureMergesIntoPendingCoordinatorTask(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 2)

	var pendingID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority,
			originator_user_id, accountable_user_id, originator_source,
			trigger_evidence_kind, trigger_evidence_ref_id
		)
		VALUES ($1, $2, $3, 'queued', 0, $4, $4, 'direct_human', 'issue_assignment', $3)
		RETURNING id`, f.coordinator, f.runtimeID, f.issueID, f.userID).Scan(&pendingID); err != nil {
		t.Fatalf("seed pending coordinator task: %v", err)
	}

	if _, err := svc.FailTask(ctx, failedID, "worker exited", "", "", "agent_error.process_failure", false, ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var triggerID pgtype.UUID
	var evidenceKind string
	if err := f.pool.QueryRow(ctx, `SELECT trigger_comment_id, trigger_evidence_kind FROM agent_task_queue WHERE id = $1`, pendingID).
		Scan(&triggerID, &evidenceKind); err != nil {
		t.Fatalf("read pending coordinator task: %v", err)
	}
	if !triggerID.Valid {
		t.Fatal("pending coordinator task did not receive the recovery comment")
	}
	if evidenceKind != string(attribution.EvidenceIssueAssignment) {
		t.Fatalf("pending task attribution changed to %q, want issue_assignment", evidenceKind)
	}
	var newRecoveryTasks int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE trigger_evidence_kind = 'delegated_failure' AND trigger_evidence_ref_id = $1`, failedID).Scan(&newRecoveryTasks); err != nil {
		t.Fatalf("count standalone recovery tasks: %v", err)
	}
	if newRecoveryTasks != 0 {
		t.Fatalf("standalone recovery tasks = %d, want 0 after merge", newRecoveryTasks)
	}

	// A second delegated failure while the same coordinator task is still
	// queued must coalesce into that task instead of creating a parallel run.
	secondFailedID := f.insertWorkerTask(t, "running", "comment", 1, 2)
	if _, err := svc.FailTask(ctx, secondFailedID, "second worker exited", "", "", "agent_error.process_failure", false, ""); err != nil {
		t.Fatalf("FailTask(second): %v", err)
	}
	var secondCommentID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `SELECT id FROM comment WHERE type = 'progress_update' AND source_task_id = $1`, secondFailedID).Scan(&secondCommentID); err != nil {
		t.Fatalf("load second recovery comment: %v", err)
	}
	var newestTrigger, coversFirst bool
	if err := f.pool.QueryRow(ctx, `
		SELECT trigger_comment_id = $2::uuid, $3::uuid = ANY(coalesced_comment_ids)
		FROM agent_task_queue WHERE id = $1`, pendingID, secondCommentID, triggerID).Scan(&newestTrigger, &coversFirst); err != nil {
		t.Fatalf("read second merged signal: %v", err)
	}
	if !newestTrigger || !coversFirst {
		t.Fatalf("parallel recovery plan = newest trigger %v / prior coalesced %v, want true/true", newestTrigger, coversFirst)
	}
}

func TestDelegatedFailurePlannedBehindDispatchedCoordinatorGetsFollowUp(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	failedID := f.insertWorkerTask(t, "running", "comment", 1, 2)

	var activeID pgtype.UUID
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (
			agent_id, runtime_id, issue_id, status, priority, dispatched_at,
			originator_user_id, accountable_user_id, originator_source
		)
		VALUES ($1, $2, $3, 'dispatched', 0, now(), $4, $4, 'direct_human')
		RETURNING id`, f.coordinator, f.runtimeID, f.issueID, f.userID).Scan(&activeID); err != nil {
		t.Fatalf("seed active coordinator task: %v", err)
	}

	if _, err := svc.FailTask(ctx, failedID, "worker exited", "", "", "agent_error.process_failure", false, ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}
	comment, err := svc.Queries.GetDelegatedFailureRecoveryComment(ctx, db.GetDelegatedFailureRecoveryCommentParams{
		IssueID:      util.MustParseUUID(f.issueID),
		WorkspaceID:  util.MustParseUUID(f.workspaceID),
		SourceTaskID: failedID,
	})
	if err != nil {
		t.Fatalf("load recovery comment: %v", err)
	}
	var planned bool
	if err := f.pool.QueryRow(ctx, `SELECT $2::uuid = ANY(coalesced_comment_ids) FROM agent_task_queue WHERE id = $1`, activeID, comment.ID).Scan(&planned); err != nil {
		t.Fatalf("read active recovery plan: %v", err)
	}
	if !planned {
		t.Fatal("active coordinator task did not record planned recovery input")
	}
	reconcilable, err := svc.Queries.ListReconcilableCommentsForIssueSince(ctx, db.ListReconcilableCommentsForIssueSinceParams{
		IssueID:           util.MustParseUUID(f.issueID),
		Since:             pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		PlannedCommentIds: []pgtype.UUID{comment.ID},
	})
	if err != nil {
		t.Fatalf("list reconcilable recovery comments: %v", err)
	}
	if len(reconcilable) != 1 || reconcilable[0].ID != comment.ID {
		t.Fatalf("reconcilable recovery comments = %+v, want only %s", reconcilable, util.UUIDToString(comment.ID))
	}

	if _, err := f.pool.Exec(ctx, `UPDATE agent_task_queue SET status = 'completed', completed_at = now() WHERE id = $1`, activeID); err != nil {
		t.Fatalf("complete coordinator task: %v", err)
	}
	if err := svc.DispatchDelegatedFailureRecoveryComment(ctx, comment, activeID); err != nil {
		t.Fatalf("replay recovery after completion: %v", err)
	}
	var followUps int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE agent_id = $1 AND issue_id = $2 AND trigger_evidence_kind = 'delegated_failure'
		  AND trigger_evidence_ref_id = $3`, f.coordinator, f.issueID, failedID).Scan(&followUps); err != nil {
		t.Fatalf("count recovery follow-ups: %v", err)
	}
	if followUps != 1 {
		t.Fatalf("recovery follow-ups = %d, want 1", followUps)
	}
}

func TestDelegatedFailureRecoveryTaskDoesNotRecursivelyWake(t *testing.T) {
	f, svc := seedDelegatedFailureFixture(t)
	ctx := context.Background()
	recoveryID := f.insertWorkerTask(t, "running", string(attribution.EvidenceDelegatedFailure), 1, 2)

	if _, err := svc.FailTask(ctx, recoveryID, "recovery failed", "", "", "agent_error.process_failure", false, ""); err != nil {
		t.Fatalf("FailTask: %v", err)
	}

	var comments int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM comment WHERE type = 'progress_update' AND source_task_id = $1`, recoveryID).Scan(&comments); err != nil {
		t.Fatalf("count recursive recovery comments: %v", err)
	}
	if comments != 0 {
		t.Fatalf("recursive recovery comments = %d, want 0", comments)
	}
}
