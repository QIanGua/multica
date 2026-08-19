package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/multica-ai/multica/server/internal/service"
)

const (
	autopilotQuotaReconcileInterval = time.Minute
	autopilotQuotaRecoveryAge       = 10 * time.Minute
	autopilotQuotaReconcileBatch    = 100
)

func runAutopilotQuotaReconciler(ctx context.Context, svc *service.AutopilotService) {
	ticker := time.NewTicker(autopilotQuotaReconcileInterval)
	defer ticker.Stop()
	for {
		if settled, err := svc.ReconcileAutopilotQuotaReservations(ctx, time.Now().Add(-autopilotQuotaRecoveryAge), autopilotQuotaReconcileBatch); err != nil {
			if ctx.Err() == nil {
				slog.Warn("autopilot quota reconciler failed", "error", err)
			}
		} else if settled > 0 {
			slog.Info("autopilot quota reconciler settled reservations", "count", settled)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
