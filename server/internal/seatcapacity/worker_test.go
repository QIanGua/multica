package seatcapacity

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type workerTestExecutor struct {
	decision Decision
	err      error
}

func (e *workerTestExecutor) Enabled() bool { return true }
func (e *workerTestExecutor) ReserveInvitation(context.Context, uuid.UUID, uuid.UUID, time.Time) (Decision, error) {
	return Decision{}, nil
}
func (e *workerTestExecutor) ClaimShareJoin(context.Context, uuid.UUID, uuid.UUID) (Decision, error) {
	return Decision{}, nil
}
func (e *workerTestExecutor) Consume(context.Context, uuid.UUID, uuid.UUID) (Decision, error) {
	return Decision{}, nil
}
func (e *workerTestExecutor) Confirm(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (Decision, error) {
	return e.decision, e.err
}
func (e *workerTestExecutor) Release(context.Context, uuid.UUID, uuid.UUID) (Decision, error) {
	return e.decision, e.err
}
func (e *workerTestExecutor) ReleaseMember(context.Context, uuid.UUID, uuid.UUID) (Decision, error) {
	return e.decision, e.err
}
func (e *workerTestExecutor) GetOperation(context.Context, uuid.UUID, uuid.UUID) (Decision, error) {
	return e.decision, e.err
}

type workerTestQueries struct {
	mu sync.Mutex

	intent          db.SeatCapacityOutbox
	claimAvailable  bool
	invitation      db.WorkspaceInvitation
	invitationError error
	stats           []db.SeatCapacityOutboxStatsRow

	transitions int
	deletes     int
	expires     int
	failures    int
	deadLetters int
}

func (q *workerTestQueries) ClaimNextDueSeatCapacityIntent(context.Context, pgtype.Timestamptz) (db.SeatCapacityOutbox, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.claimAvailable {
		return db.SeatCapacityOutbox{}, pgx.ErrNoRows
	}
	q.claimAvailable = false
	return q.intent, nil
}

func (q *workerTestQueries) DeleteSeatCapacityIntentForAction(_ context.Context, arg db.DeleteSeatCapacityIntentForActionParams) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.intent.OperationToken == arg.OperationToken && q.intent.Action == arg.Action {
		q.deletes++
	}
	return nil
}

func (q *workerTestQueries) ExpireInvitationForCapacityRecovery(context.Context, pgtype.UUID) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expires++
	return nil
}

func (q *workerTestQueries) GetInvitation(context.Context, pgtype.UUID) (db.WorkspaceInvitation, error) {
	return q.invitation, q.invitationError
}

func (q *workerTestQueries) MarkSeatCapacityIntentDeadLettered(context.Context, db.MarkSeatCapacityIntentDeadLetteredParams) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deadLetters++
	return 1, nil
}

func (q *workerTestQueries) MarkSeatCapacityIntentFailed(context.Context, db.MarkSeatCapacityIntentFailedParams) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failures++
	return nil
}

func (q *workerTestQueries) SeatCapacityOutboxStats(context.Context) ([]db.SeatCapacityOutboxStatsRow, error) {
	return q.stats, nil
}

func (q *workerTestQueries) TransitionSeatCapacityIntent(_ context.Context, arg db.TransitionSeatCapacityIntentParams) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.intent.OperationToken != arg.OperationToken || q.intent.Action != arg.CurrentAction {
		return 0, nil
	}
	q.intent.Action = arg.NextAction
	q.transitions++
	return 1, nil
}

func (q *workerTestQueries) counts() (transitions, deletes, expires, failures, deadLetters int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.transitions, q.deletes, q.expires, q.failures, q.deadLetters
}

func workerTestIntent(action string) db.SeatCapacityOutbox {
	return db.SeatCapacityOutbox{
		WorkspaceID: uuidToTestPG(uuid.New()), OperationToken: uuidToTestPG(uuid.New()),
		Action: action, InvitationID: uuidToTestPG(uuid.New()),
	}
}

func uuidToTestPG(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: value, Valid: true}
}

func recoveredDecision(state string) Decision {
	return Decision{Managed: true, Operation: &Operation{State: state}}
}

func TestRecoverConsumingTransitionsAbandonedOperationToRelease(t *testing.T) {
	intent := workerTestIntent(ActionConsumeInvitation)
	queries := &workerTestQueries{intent: intent}
	worker := newWorker(queries, &workerTestExecutor{decision: recoveredDecision("consuming")}, WorkerConfig{})

	if err := worker.recoverConsuming(context.Background(), intent, uuidFromPG(intent.WorkspaceID), uuidFromPG(intent.OperationToken)); err != nil {
		t.Fatal(err)
	}
	transitions, deletes, expires, _, _ := queries.counts()
	if transitions != 1 || deletes != 0 || expires != 1 {
		t.Fatalf("transitions=%d deletes=%d expires=%d, want 1/0/1", transitions, deletes, expires)
	}
	if queries.intent.Action != ActionRelease {
		t.Fatalf("action=%q, want %q", queries.intent.Action, ActionRelease)
	}
}

func TestRecoverConsumingUsedDeletesWithoutReleasing(t *testing.T) {
	intent := workerTestIntent(ActionConsumeInvitation)
	queries := &workerTestQueries{intent: intent}
	worker := newWorker(queries, &workerTestExecutor{decision: recoveredDecision("used")}, WorkerConfig{})

	if err := worker.recoverConsuming(context.Background(), intent, uuidFromPG(intent.WorkspaceID), uuidFromPG(intent.OperationToken)); err != nil {
		t.Fatal(err)
	}
	transitions, deletes, expires, _, _ := queries.counts()
	if transitions != 0 || deletes != 1 || expires != 0 {
		t.Fatalf("transitions=%d deletes=%d expires=%d, want 0/1/0", transitions, deletes, expires)
	}
}

func TestRecoverReserveKeepsPendingInvitationReservation(t *testing.T) {
	intent := workerTestIntent(ActionReserveInvitation)
	queries := &workerTestQueries{
		intent: intent,
		invitation: db.WorkspaceInvitation{
			ID: intent.InvitationID, Status: "pending",
		},
	}
	worker := newWorker(queries, &workerTestExecutor{decision: recoveredDecision("reserved")}, WorkerConfig{})

	if err := worker.recoverReserve(context.Background(), intent, uuidFromPG(intent.WorkspaceID), uuidFromPG(intent.OperationToken)); err != nil {
		t.Fatal(err)
	}
	transitions, deletes, expires, _, _ := queries.counts()
	if transitions != 0 || deletes != 1 || expires != 0 {
		t.Fatalf("transitions=%d deletes=%d expires=%d, want 0/1/0", transitions, deletes, expires)
	}
}

func TestRecoveryCleansUnknownOrUnmanagedOperations(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		err      error
	}{
		{name: "not found", err: &HTTPError{StatusCode: http.StatusNotFound}},
		{name: "unmanaged", decision: Decision{Managed: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := workerTestIntent(ActionConsumeInvitation)
			queries := &workerTestQueries{intent: intent}
			worker := newWorker(queries, &workerTestExecutor{decision: tt.decision, err: tt.err}, WorkerConfig{})

			if err := worker.recoverConsuming(context.Background(), intent, uuidFromPG(intent.WorkspaceID), uuidFromPG(intent.OperationToken)); err != nil {
				t.Fatal(err)
			}
			_, deletes, expires, _, _ := queries.counts()
			if deletes != 1 || expires != 1 {
				t.Fatalf("deletes=%d expires=%d, want 1/1", deletes, expires)
			}
		})
	}
}

func TestConcurrentRecoveryOnlyOneReplicaTransitionsIntent(t *testing.T) {
	intent := workerTestIntent(ActionConsumeInvitation)
	queries := &workerTestQueries{intent: intent}
	executor := &workerTestExecutor{decision: recoveredDecision("consuming")}
	workerA := newWorker(queries, executor, WorkerConfig{})
	workerB := newWorker(queries, executor, WorkerConfig{})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, worker := range []*Worker{workerA, workerB} {
		wg.Add(1)
		go func(w *Worker) {
			defer wg.Done()
			errs <- w.recoverConsuming(context.Background(), intent, uuidFromPG(intent.WorkspaceID), uuidFromPG(intent.OperationToken))
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	transitions, _, expires, _, _ := queries.counts()
	if transitions != 1 || expires != 1 {
		t.Fatalf("transitions=%d expires=%d, want 1/1", transitions, expires)
	}
}

func TestWorkerDeadLettersAfterMaximumAttempts(t *testing.T) {
	intent := workerTestIntent(ActionConfirm)
	intent.AttemptCount = 1
	queries := &workerTestQueries{intent: intent, claimAvailable: true}
	worker := newWorker(queries, &workerTestExecutor{err: errors.New("cloud unavailable")}, WorkerConfig{
		MaxAttempts: 2,
	})

	if err := worker.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, _, failures, deadLetters := queries.counts()
	if failures != 0 || deadLetters != 1 {
		t.Fatalf("failures=%d deadLetters=%d, want 0/1", failures, deadLetters)
	}
}

func TestRecoveryDueAllowsRetryableRequestFailuresToSettle(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := RecoveryDue(now).Time.Sub(now); got != 5*time.Minute {
		t.Fatalf("RecoveryDue delay=%s, want 5m", got)
	}
}
