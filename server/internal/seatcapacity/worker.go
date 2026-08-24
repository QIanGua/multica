package seatcapacity

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	ActionReserveInvitation = "reserve_invitation"
	ActionConsumeInvitation = "consume_invitation"
	ActionClaimShareJoin    = "claim_share_join"
	ActionConfirm           = "confirm"
	ActionRelease           = "release"
	ActionReleaseMember     = "release_member"

	defaultReconcileInterval = 30 * time.Second
	defaultRecoveryGrace     = 2 * time.Minute
	defaultBatchSize         = 100
)

type WorkerConfig struct {
	ReconcileInterval time.Duration
	BatchSize         int32
	Logger            *slog.Logger
}

// Worker settles durable product-side intents. The Cloud operations are
// idempotent, so multiple API replicas may process the same row safely.
type Worker struct {
	queries           *db.Queries
	executor          Executor
	reconcileInterval time.Duration
	batchSize         int32
	logger            *slog.Logger
	now               func() time.Time
}

func NewWorker(queries *db.Queries, executor Executor, cfg WorkerConfig) *Worker {
	interval := cfg.ReconcileInterval
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = defaultBatchSize
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		queries: queries, executor: executor, reconcileInterval: interval,
		batchSize: batch, logger: logger, now: time.Now,
	}
}

func (w *Worker) Enabled() bool {
	return w != nil && w.queries != nil && w.executor != nil && w.executor.Enabled()
}

func (w *Worker) Run(ctx context.Context) {
	if !w.Enabled() {
		return
	}
	ticker := time.NewTicker(w.reconcileInterval)
	defer ticker.Stop()
	for {
		if err := w.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			w.logger.WarnContext(ctx, "seat capacity outbox reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) ReconcileOnce(ctx context.Context) error {
	if !w.Enabled() {
		return nil
	}
	intents, err := w.queries.ListDueSeatCapacityIntents(ctx, w.batchSize)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := w.settle(ctx, intent); err != nil {
			w.recordFailure(ctx, intent, err)
		}
	}
	return nil
}

func (w *Worker) settle(ctx context.Context, intent db.SeatCapacityOutbox) error {
	workspaceID := uuidFromPG(intent.WorkspaceID)
	token := uuidFromPG(intent.OperationToken)
	switch intent.Action {
	case ActionReserveInvitation:
		return w.recoverReserve(ctx, intent, workspaceID, token)
	case ActionConsumeInvitation, ActionClaimShareJoin:
		return w.recoverConsuming(ctx, intent, workspaceID, token)
	case ActionConfirm:
		decision, err := w.executor.Confirm(ctx, workspaceID, token, uuidFromPG(intent.MemberID))
		if err != nil {
			return err
		}
		if !decision.Allowed {
			return errors.New("capacity confirm rejected in state " + decision.Reason)
		}
		return w.deleteCurrent(ctx, intent)
	case ActionRelease:
		decision, err := w.executor.Release(ctx, workspaceID, token)
		if err != nil && !IsNotFound(err) {
			return err
		}
		if err == nil && decision.Managed && !decision.Allowed && decision.Reason != "released" && decision.Reason != "denied" {
			return errors.New("capacity release rejected in state " + decision.Reason)
		}
		return w.deleteCurrent(ctx, intent)
	case ActionReleaseMember:
		decision, err := w.executor.ReleaseMember(ctx, workspaceID, uuidFromPG(intent.MemberID))
		if err != nil && !IsNotFound(err) {
			return err
		}
		if err == nil && decision.Managed && !decision.Allowed && decision.Reason != "released" {
			return errors.New("capacity member release rejected in state " + decision.Reason)
		}
		return w.deleteCurrent(ctx, intent)
	default:
		return errors.New("unknown seat capacity outbox action")
	}
}

func (w *Worker) recoverReserve(ctx context.Context, intent db.SeatCapacityOutbox, workspaceID, token uuid.UUID) error {
	decision, err := w.executor.GetOperation(ctx, workspaceID, token)
	if IsNotFound(err) || (err == nil && !decision.Managed) {
		return w.deleteCurrent(ctx, intent)
	}
	if err != nil {
		return err
	}
	if decision.Operation == nil {
		return errors.New("managed capacity operation response omitted operation")
	}
	switch decision.Operation.State {
	case "denied", "released":
		return w.deleteCurrent(ctx, intent)
	case "reserved":
		invitationID := intent.InvitationID
		if !invitationID.Valid {
			_, transitionErr := w.transition(ctx, intent, ActionRelease, pgtype.UUID{})
			return transitionErr
		}
		invitation, getErr := w.queries.GetInvitation(ctx, invitationID)
		if getErr == nil && invitation.Status == "pending" {
			return w.deleteCurrent(ctx, intent)
		}
		if getErr != nil && !errors.Is(getErr, pgx.ErrNoRows) {
			return getErr
		}
		_, transitionErr := w.transition(ctx, intent, ActionRelease, pgtype.UUID{})
		return transitionErr
	default:
		return errors.New("unexpected recovered invitation reservation state " + decision.Operation.State)
	}
}

func (w *Worker) recoverConsuming(ctx context.Context, intent db.SeatCapacityOutbox, workspaceID, token uuid.UUID) error {
	decision, err := w.executor.GetOperation(ctx, workspaceID, token)
	if IsNotFound(err) || (err == nil && !decision.Managed) {
		if intent.Action == ActionConsumeInvitation && intent.InvitationID.Valid {
			_ = w.queries.ExpireInvitationForCapacityRecovery(ctx, intent.InvitationID)
		}
		return w.deleteCurrent(ctx, intent)
	}
	if err != nil {
		return err
	}
	if decision.Operation == nil {
		return errors.New("managed capacity operation response omitted operation")
	}
	switch decision.Operation.State {
	case "reserved":
		// The consume request never took effect. Keep the invitation usable.
		return w.deleteCurrent(ctx, intent)
	case "used":
		// A concurrent request already committed and confirmed the member. A
		// stale consuming worker must not try to release that used seat.
		return w.deleteCurrent(ctx, intent)
	case "denied", "released":
		if intent.Action == ActionConsumeInvitation && intent.InvitationID.Valid {
			_ = w.queries.ExpireInvitationForCapacityRecovery(ctx, intent.InvitationID)
		}
		return w.deleteCurrent(ctx, intent)
	case "consuming":
		// No product transaction committed: that transaction would atomically
		// change this row to confirm. Retire the abandoned user request rather
		// than hold capacity forever.
		changed, err := w.transition(ctx, intent, ActionRelease, pgtype.UUID{})
		if err != nil || !changed {
			return err
		}
		if intent.Action == ActionConsumeInvitation && intent.InvitationID.Valid {
			if err := w.queries.ExpireInvitationForCapacityRecovery(ctx, intent.InvitationID); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("unexpected recovered consuming capacity state " + decision.Operation.State)
	}
}

func (w *Worker) transition(ctx context.Context, intent db.SeatCapacityOutbox, action string, memberID pgtype.UUID) (bool, error) {
	rows, err := w.queries.TransitionSeatCapacityIntent(ctx, db.TransitionSeatCapacityIntentParams{
		NextAction: action, CurrentAction: intent.Action, MemberID: memberID, OperationToken: intent.OperationToken,
		NextAttemptAt: pgtype.Timestamptz{Time: w.now(), Valid: true},
	})
	return rows == 1, err
}

func (w *Worker) deleteCurrent(ctx context.Context, intent db.SeatCapacityOutbox) error {
	return w.queries.DeleteSeatCapacityIntentForAction(ctx, db.DeleteSeatCapacityIntentForActionParams{
		OperationToken: intent.OperationToken,
		Action:         intent.Action,
	})
}

func (w *Worker) recordFailure(ctx context.Context, intent db.SeatCapacityOutbox, settleErr error) {
	backoff := 5 * time.Second
	for i := int32(0); i < intent.AttemptCount && backoff < 5*time.Minute; i++ {
		backoff *= 2
	}
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	err := w.queries.MarkSeatCapacityIntentFailed(ctx, db.MarkSeatCapacityIntentFailedParams{
		LastError: settleErr.Error(), NextAttemptAt: pgtype.Timestamptz{Time: w.now().Add(backoff), Valid: true},
		OperationToken: intent.OperationToken, Action: intent.Action,
	})
	if err != nil {
		w.logger.WarnContext(ctx, "seat capacity outbox failure could not be recorded", "error", err)
	}
	if intent.AttemptCount == 0 || (intent.AttemptCount+1)%10 == 0 {
		w.logger.WarnContext(ctx, "seat capacity outbox intent remains unsettled",
			"workspace_id", workspaceIDString(intent.WorkspaceID), "action", intent.Action,
			"attempt", intent.AttemptCount+1, "error", settleErr)
	}
}

func uuidFromPG(value pgtype.UUID) uuid.UUID {
	if !value.Valid {
		return uuid.Nil
	}
	return uuid.UUID(value.Bytes)
}

func workspaceIDString(value pgtype.UUID) string { return uuidFromPG(value).String() }

func RecoveryDue(now time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: now.Add(defaultRecoveryGrace), Valid: true}
}

func RetryDue(now time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: now, Valid: true}
}
