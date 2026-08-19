package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// AutopilotQuotaMetrics deliberately accepts only bounded labels. Workspace
// identifiers and policy values must never be metric labels.
type AutopilotQuotaMetrics interface {
	RecordAutopilotQuotaDecision(action, source, result string)
}

// AutopilotQuotaExceededError is returned only for an enforce decision whose
// Cloud-provided interval is full. HTTP callers can serialize the facts without
// embedding commercial copy or plan names in OSS.
type AutopilotQuotaExceededError struct {
	Used     int64
	Reserved int64
	Limit    int64
	ResetAt  time.Time
}

func (e *AutopilotQuotaExceededError) Error() string {
	return "autopilot run quota exceeded"
}

// AutopilotQuotaUsage is the workspace-scoped, policy-neutral API model.
// A disabled/malformed decision returns Enabled=false and leaves all facts nil.
type AutopilotQuotaUsage struct {
	Enabled     bool
	Action      string
	Used        *int64
	Reserved    *int64
	Limit       *int64
	PeriodStart *time.Time
	PeriodEnd   *time.Time
	ResetAt     *time.Time
}

type autopilotQuotaPolicy struct {
	action              entitlement.Action
	limit               int64
	periodStart         time.Time
	periodEnd           time.Time
	resetAt             time.Time
	policyRevision      int64
	subscriptionVersion int64
}

func newAutopilotIdempotencyKey() string { return uuid.NewString() }

// NewRequestIdempotencyKey is used only when an HTTP caller omitted its key;
// the generated value scopes idempotency to that single request.
func NewRequestIdempotencyKey() string { return newAutopilotIdempotencyKey() }

func (s *AutopilotService) quotaPolicy(ctx context.Context, workspaceID pgtype.UUID) (autopilotQuotaPolicy, bool) {
	if s.Entitlements == nil || !workspaceID.Valid {
		return autopilotQuotaPolicy{}, false
	}
	decision := s.Entitlements.Gate(ctx, uuid.UUID(workspaceID.Bytes), entitlement.GateAutopilotRuns)
	gate := decision.Gate
	if gate.Action == entitlement.ActionOff {
		return autopilotQuotaPolicy{}, false
	}
	if (gate.Action != entitlement.ActionObserve && gate.Action != entitlement.ActionEnforce) ||
		gate.Limit == nil || *gate.Limit < 0 || gate.PeriodStart == nil || gate.PeriodEnd == nil ||
		gate.ResetAt == nil || !gate.PeriodStart.Before(*gate.PeriodEnd) {
		// A malformed policy is fail-open and, critically, performs no quota-table
		// access. Cloud remains the sole authority over interval construction.
		return autopilotQuotaPolicy{}, false
	}
	return autopilotQuotaPolicy{
		action:              gate.Action,
		limit:               int64(*gate.Limit),
		periodStart:         gate.PeriodStart.UTC(),
		periodEnd:           gate.PeriodEnd.UTC(),
		resetAt:             gate.ResetAt.UTC(),
		policyRevision:      decision.PolicyRevision,
		subscriptionVersion: decision.SubscriptionVersion,
	}, true
}

// createAutopilotRunWithQuota reserves and links a run in one transaction.
// When policy is off, it intentionally uses the legacy direct INSERT so a
// self-hosted deployment never touches the quota tables.
func (s *AutopilotService) createAutopilotRunWithQuota(
	ctx context.Context,
	workspaceID pgtype.UUID,
	source, idempotencyKey string,
	params db.CreateAutopilotRunParams,
) (db.AutopilotRun, bool, error) {
	policy, enabled := s.quotaPolicy(ctx, workspaceID)
	if !enabled {
		run, err := s.Queries.CreateAutopilotRun(ctx, params)
		return run, false, err
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AutopilotRun{}, false, fmt.Errorf("begin quota admission: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	periodArgs := db.EnsureAutopilotQuotaPeriodParams{
		WorkspaceID: workspaceID,
		PeriodStart: pgtype.Timestamptz{Time: policy.periodStart, Valid: true},
		PeriodEnd:   pgtype.Timestamptz{Time: policy.periodEnd, Valid: true},
	}
	period, err := qtx.EnsureAutopilotQuotaPeriod(ctx, periodArgs)
	if err != nil {
		return db.AutopilotRun{}, false, fmt.Errorf("lock quota period: %w", err)
	}

	existing, err := qtx.GetAutopilotQuotaReservationByKey(ctx, db.GetAutopilotQuotaReservationByKeyParams{
		WorkspaceID:    workspaceID,
		PeriodStart:    periodArgs.PeriodStart,
		PeriodEnd:      periodArgs.PeriodEnd,
		IdempotencyKey: idempotencyKey,
	})
	if err == nil {
		run, runErr := qtx.GetAutopilotRunByQuotaReservation(ctx, existing.ID)
		if runErr != nil {
			return db.AutopilotRun{}, false, fmt.Errorf("load idempotent quota run: %w", runErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return db.AutopilotRun{}, false, fmt.Errorf("commit idempotent quota admission: %w", err)
		}
		s.recordAutopilotQuotaDecision(policy.action, source, "reused")
		return run, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return db.AutopilotRun{}, false, fmt.Errorf("lookup quota reservation: %w", err)
	}

	wouldBlock := period.UsedCount+period.ReservedCount >= policy.limit
	if wouldBlock && policy.action == entitlement.ActionEnforce {
		if _, err := qtx.IncrementAutopilotQuotaBlocked(ctx, db.IncrementAutopilotQuotaBlockedParams{
			Source: source, WorkspaceID: workspaceID,
			PeriodStart: periodArgs.PeriodStart, PeriodEnd: periodArgs.PeriodEnd,
		}); err != nil {
			return db.AutopilotRun{}, false, fmt.Errorf("record blocked quota admission: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return db.AutopilotRun{}, false, fmt.Errorf("commit blocked quota admission: %w", err)
		}
		s.recordAutopilotQuotaDecision(policy.action, source, "blocked")
		return db.AutopilotRun{}, false, &AutopilotQuotaExceededError{
			Used: period.UsedCount, Reserved: period.ReservedCount,
			Limit: policy.limit, ResetAt: policy.resetAt,
		}
	}
	if wouldBlock {
		if _, err := qtx.IncrementAutopilotQuotaWouldBlock(ctx, db.IncrementAutopilotQuotaWouldBlockParams{
			Source: source, WorkspaceID: workspaceID,
			PeriodStart: periodArgs.PeriodStart, PeriodEnd: periodArgs.PeriodEnd,
		}); err != nil {
			return db.AutopilotRun{}, false, fmt.Errorf("record observed quota admission: %w", err)
		}
	}

	reservation, err := qtx.CreateAutopilotQuotaReservation(ctx, db.CreateAutopilotQuotaReservationParams{
		WorkspaceID: workspaceID, PeriodStart: periodArgs.PeriodStart, PeriodEnd: periodArgs.PeriodEnd,
		PolicyRevision: policy.policyRevision, SubscriptionVersion: policy.subscriptionVersion,
		Source: source, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return db.AutopilotRun{}, false, fmt.Errorf("create quota reservation: %w", err)
	}
	if _, err := qtx.IncrementAutopilotQuotaReserved(ctx, db.IncrementAutopilotQuotaReservedParams{
		WorkspaceID: periodArgs.WorkspaceID, PeriodStart: periodArgs.PeriodStart, PeriodEnd: periodArgs.PeriodEnd,
	}); err != nil {
		return db.AutopilotRun{}, false, fmt.Errorf("increment reserved quota: %w", err)
	}
	params.QuotaReservationID = reservation.ID
	run, err := qtx.CreateAutopilotRun(ctx, params)
	if err != nil {
		return db.AutopilotRun{}, false, fmt.Errorf("create quota-linked run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AutopilotRun{}, false, fmt.Errorf("commit quota admission: %w", err)
	}
	result := "admitted"
	if wouldBlock {
		result = "would_block"
	}
	s.recordAutopilotQuotaDecision(policy.action, source, result)
	return run, false, nil
}

func (s *AutopilotService) recordAutopilotQuotaDecision(action entitlement.Action, source, result string) {
	if s.QuotaMetrics != nil {
		s.QuotaMetrics.RecordAutopilotQuotaDecision(string(action), source, result)
	}
}

func settleAutopilotQuota(ctx context.Context, q *db.Queries, reservationID pgtype.UUID, consume bool) error {
	if !reservationID.Valid {
		return nil
	}
	var err error
	if consume {
		_, err = q.ConsumeAutopilotQuotaReservation(ctx, reservationID)
	} else {
		_, err = q.ReleaseAutopilotQuotaReservation(ctx, reservationID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // terminal replay: reservation already finalized
	}
	return err
}

func (s *AutopilotService) completeAutopilotRun(ctx context.Context, params db.UpdateAutopilotRunCompletedParams) (db.AutopilotRun, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AutopilotRun{}, err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	run, err := qtx.UpdateAutopilotRunCompleted(ctx, params)
	if err != nil {
		return db.AutopilotRun{}, err
	}
	if err := settleAutopilotQuota(ctx, qtx, run.QuotaReservationID, true); err != nil {
		return db.AutopilotRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AutopilotRun{}, err
	}
	return run, nil
}

func (s *AutopilotService) failAutopilotRun(ctx context.Context, params db.UpdateAutopilotRunFailedParams) (db.AutopilotRun, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AutopilotRun{}, err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	run, err := qtx.UpdateAutopilotRunFailed(ctx, params)
	if err != nil {
		return db.AutopilotRun{}, err
	}
	if err := settleAutopilotQuota(ctx, qtx, run.QuotaReservationID, false); err != nil {
		return db.AutopilotRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AutopilotRun{}, err
	}
	return run, nil
}

func (s *AutopilotService) skipAutopilotRun(ctx context.Context, params db.UpdateAutopilotRunSkippedParams) (db.AutopilotRun, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return db.AutopilotRun{}, err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	run, err := qtx.UpdateAutopilotRunSkipped(ctx, params)
	if err != nil {
		return db.AutopilotRun{}, err
	}
	if err := settleAutopilotQuota(ctx, qtx, run.QuotaReservationID, false); err != nil {
		return db.AutopilotRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.AutopilotRun{}, err
	}
	return run, nil
}

func (s *AutopilotService) recoverPartialAutopilotRun(ctx context.Context, run db.AutopilotRun) error {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	if err := qtx.RecoverPartialAutopilotRun(ctx, run.ID); err != nil {
		return err
	}
	if err := settleAutopilotQuota(ctx, qtx, run.QuotaReservationID, false); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FailAutopilotRunsByIssue compensates create_issue quota consumption before
// deletion clears issue_id via ON DELETE SET NULL.
func (s *AutopilotService) FailAutopilotRunsByIssue(ctx context.Context, issueID pgtype.UUID) error {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	qtx := s.Queries.WithTx(tx)
	runs, err := qtx.FailAutopilotRunsByIssue(ctx, issueID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if err := settleAutopilotQuota(ctx, qtx, run.QuotaReservationID, false); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *AutopilotService) AutopilotQuotaUsage(ctx context.Context, workspaceID pgtype.UUID) (AutopilotQuotaUsage, error) {
	policy, enabled := s.quotaPolicy(ctx, workspaceID)
	if !enabled {
		return AutopilotQuotaUsage{Enabled: false}, nil
	}
	period, err := s.Queries.GetAutopilotQuotaPeriod(ctx, db.GetAutopilotQuotaPeriodParams{
		WorkspaceID: workspaceID,
		PeriodStart: pgtype.Timestamptz{Time: policy.periodStart, Valid: true},
		PeriodEnd:   pgtype.Timestamptz{Time: policy.periodEnd, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		period.UsedCount = 0
		period.ReservedCount = 0
	} else if err != nil {
		return AutopilotQuotaUsage{}, fmt.Errorf("load autopilot quota usage: %w", err)
	}
	return AutopilotQuotaUsage{
		Enabled: true, Action: string(policy.action),
		Used: &period.UsedCount, Reserved: &period.ReservedCount, Limit: &policy.limit,
		PeriodStart: &policy.periodStart, PeriodEnd: &policy.periodEnd, ResetAt: &policy.resetAt,
	}, nil
}

func (s *AutopilotService) QuotaEnabled() bool { return s.Entitlements != nil }

// ReconcileAutopilotQuotaReservations repairs crash windows left after a
// reservation/run transaction but before the downstream side effect or normal
// finalizer. The reservation transition remains CAS-based, so replicas may run
// this concurrently without double-adjusting counters.
func (s *AutopilotService) ReconcileAutopilotQuotaReservations(ctx context.Context, createdBefore time.Time, limit int32) (int, error) {
	if !s.QuotaEnabled() || limit <= 0 {
		return 0, nil
	}
	reservations, err := s.Queries.ListRecoverableAutopilotQuotaReservations(ctx, db.ListRecoverableAutopilotQuotaReservationsParams{
		CreatedBefore: pgtype.Timestamptz{Time: createdBefore.UTC(), Valid: true},
		RowLimit:      limit,
	})
	if err != nil {
		return 0, fmt.Errorf("list recoverable quota reservations: %w", err)
	}
	settled := 0
	for _, reservation := range reservations {
		run, runErr := s.Queries.GetAutopilotRunByQuotaReservation(ctx, reservation.ID)
		switch {
		case errors.Is(runErr, pgx.ErrNoRows):
			if err := settleAutopilotQuota(ctx, s.Queries, reservation.ID, false); err != nil {
				return settled, fmt.Errorf("release orphan quota reservation: %w", err)
			}
		case runErr != nil:
			return settled, fmt.Errorf("load quota-linked run: %w", runErr)
		case run.Status == "completed":
			if err := settleAutopilotQuota(ctx, s.Queries, reservation.ID, true); err != nil {
				return settled, fmt.Errorf("consume completed quota reservation: %w", err)
			}
		case run.Status == "failed" || run.Status == "skipped":
			if err := settleAutopilotQuota(ctx, s.Queries, reservation.ID, false); err != nil {
				return settled, fmt.Errorf("release terminal quota reservation: %w", err)
			}
		default:
			if _, err := s.failAutopilotRun(ctx, db.UpdateAutopilotRunFailedParams{
				ID:            run.ID,
				FailureReason: pgtype.Text{String: "quota reservation recovery: downstream side effect missing", Valid: true},
				ReasonCode:    pgtype.Text{String: "internal_error", Valid: true},
			}); err != nil {
				return settled, fmt.Errorf("fail partial quota run: %w", err)
			}
		}
		settled++
	}
	return settled, nil
}
