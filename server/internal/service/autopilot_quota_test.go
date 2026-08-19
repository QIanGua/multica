package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/entitlement"
	"github.com/multica-ai/multica/server/internal/entitlement/entitlementtest"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestAutopilotQuotaDisabledDoesNotReadQuotaTables(t *testing.T) {
	ctx := context.Background()
	workspace := uuid.New()
	workspaceID := pgtype.UUID{Bytes: workspace, Valid: true}

	t.Run("off", func(t *testing.T) {
		stub := entitlementtest.New()
		stub.Set(workspace, entitlement.GateAutopilotRuns, entitlement.Decision{
			Gate: entitlement.Gate{Action: entitlement.ActionOff},
		})
		svc := &AutopilotService{Entitlements: stub} // Queries intentionally nil
		usage, err := svc.AutopilotQuotaUsage(ctx, workspaceID)
		if err != nil || usage.Enabled {
			t.Fatalf("off usage = %+v, %v; want disabled", usage, err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		stub := entitlementtest.New()
		limit := 2
		stub.Set(workspace, entitlement.GateAutopilotRuns, entitlement.Decision{
			Gate: entitlement.Gate{Action: entitlement.ActionEnforce, Limit: &limit},
		})
		svc := &AutopilotService{Entitlements: stub} // missing interval; Queries intentionally nil
		usage, err := svc.AutopilotQuotaUsage(ctx, workspaceID)
		if err != nil || usage.Enabled {
			t.Fatalf("malformed usage = %+v, %v; want fail-open disabled", usage, err)
		}
	})
}

func TestAutopilotQuotaEnforcesBoundaryAndFinalizesIdempotently(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceIDString, publisherID, agentID, _ := seedAttributionFixture(t, pool)
	autopilotIDString, _ := seedRunOnlyAutopilot(t, pool, workspaceIDString, agentID, publisherID)
	workspaceID := util.MustParseUUID(workspaceIDString)
	autopilotID := util.MustParseUUID(autopilotIDString)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM autopilot_quota_reservation WHERE workspace_id = $1`, workspaceID)
		pool.Exec(context.Background(), `DELETE FROM autopilot_quota_period WHERE workspace_id = $1`, workspaceID)
	})

	periodStart := time.Now().UTC().Truncate(time.Second)
	periodEnd := periodStart.Add(37 * time.Hour) // opaque Cloud interval, deliberately not a calendar month
	resetAt := periodEnd
	limit := 2
	stub := entitlementtest.New()
	stub.Set(uuid.UUID(workspaceID.Bytes), entitlement.GateAutopilotRuns, entitlement.Decision{
		Gate: entitlement.Gate{
			Action: entitlement.ActionEnforce, Limit: &limit,
			PeriodStart: &periodStart, PeriodEnd: &periodEnd, ResetAt: &resetAt,
		},
		PolicyRevision: 7, SubscriptionVersion: 11,
	})
	svc := &AutopilotService{Queries: q, TxStarter: pool, Bus: events.New(), Entitlements: stub}
	params := db.CreateAutopilotRunParams{
		AutopilotID: autopilotID, Source: "api", Status: "running",
	}

	runs := make([]db.AutopilotRun, 0, limit)
	for i, key := range []string{"boundary-1", "boundary-2"} {
		run, _, err := svc.createAutopilotRunWithQuota(ctx, workspaceID, "api", key, params)
		if err != nil {
			t.Fatalf("admission %d: %v", i+1, err)
		}
		runs = append(runs, run)
	}
	reused, wasReused, err := svc.createAutopilotRunWithQuota(ctx, workspaceID, "api", "boundary-1", params)
	if err != nil || !wasReused || reused.ID.Bytes != runs[0].ID.Bytes {
		t.Fatalf("idempotent reuse = %s, %v; want %s", util.UUIDToString(reused.ID), err, util.UUIDToString(runs[0].ID))
	}
	_, _, err = svc.createAutopilotRunWithQuota(ctx, workspaceID, "api", "boundary-3", params)
	var quotaErr *AutopilotQuotaExceededError
	if !errors.As(err, &quotaErr) {
		t.Fatalf("N+1 admission error = %v, want quota exceeded", err)
	}

	usage, err := svc.AutopilotQuotaUsage(ctx, workspaceID)
	if err != nil || usage.Used == nil || usage.Reserved == nil || *usage.Used != 0 || *usage.Reserved != int64(limit) {
		t.Fatalf("reserved usage = %+v, %v", usage, err)
	}
	if err := settleAutopilotQuota(ctx, q, runs[0].QuotaReservationID, true); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := settleAutopilotQuota(ctx, q, runs[0].QuotaReservationID, true); err != nil {
		t.Fatalf("duplicate consume: %v", err)
	}
	if err := settleAutopilotQuota(ctx, q, runs[1].QuotaReservationID, false); err != nil {
		t.Fatalf("release: %v", err)
	}
	// A create_issue run can consume at issue creation and later fail; release
	// must compensate the previously consumed unit exactly once.
	if err := settleAutopilotQuota(ctx, q, runs[0].QuotaReservationID, false); err != nil {
		t.Fatalf("compensating release: %v", err)
	}
	usage, err = svc.AutopilotQuotaUsage(ctx, workspaceID)
	if err != nil || *usage.Used != 0 || *usage.Reserved != 0 {
		t.Fatalf("final usage = %+v, %v; want zero", usage, err)
	}
}

func TestAutopilotQuotaSchedulePersistsSkippedRun(t *testing.T) {
	pool := newResolveOriginatorPool(t)
	ctx := context.Background()
	q := db.New(pool)
	workspaceIDString, publisherID, agentID, _ := seedAttributionFixture(t, pool)
	autopilotIDString, _ := seedRunOnlyAutopilot(t, pool, workspaceIDString, agentID, publisherID)
	workspaceID := util.MustParseUUID(workspaceIDString)
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM autopilot_quota_reservation WHERE workspace_id = $1`, workspaceID)
		pool.Exec(context.Background(), `DELETE FROM autopilot_quota_period WHERE workspace_id = $1`, workspaceID)
	})

	start := time.Now().UTC().Truncate(time.Second)
	end := start.Add(13 * time.Hour)
	limit := 0
	stub := entitlementtest.New()
	stub.Set(uuid.UUID(workspaceID.Bytes), entitlement.GateAutopilotRuns, entitlement.Decision{
		Gate: entitlement.Gate{
			Action: entitlement.ActionEnforce, Limit: &limit,
			PeriodStart: &start, PeriodEnd: &end, ResetAt: &end,
		},
	})
	autopilot, err := q.GetAutopilot(ctx, util.MustParseUUID(autopilotIDString))
	if err != nil {
		t.Fatalf("load autopilot: %v", err)
	}
	var triggerID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO autopilot_trigger (autopilot_id, kind, enabled, cron_expression, timezone)
		VALUES ($1, 'schedule', true, '0 * * * *', 'UTC') RETURNING id`, autopilot.ID).Scan(&triggerID); err != nil {
		t.Fatalf("create schedule trigger: %v", err)
	}
	svc := &AutopilotService{
		Queries: q, TxStarter: pool, Bus: events.New(),
		TaskSvc:      &TaskService{Queries: q, TxStarter: pool, Bus: events.New()},
		Entitlements: stub,
	}
	run, err := svc.DispatchAutopilotForPlan(ctx, autopilot, triggerID, "schedule", nil, start.Add(time.Minute))
	if err != nil {
		t.Fatalf("scheduled dispatch: %v", err)
	}
	if run.Status != "skipped" || !run.ReasonCode.Valid || run.ReasonCode.String != "quota_exceeded" {
		t.Fatalf("scheduled run = status %q reason %q, want skipped/quota_exceeded", run.Status, run.ReasonCode.String)
	}
}
