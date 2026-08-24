package seatcapacity

import (
	"testing"
	"time"
)

// cloudMemberAdoptionSettlementHorizon mirrors multica-cloud's
// memberAdoptionSettlementHorizon. Cloud waits this long before treating a
// product member row as one its ledger never recorded.
//
// Keeping a copy here is deliberate: the two services deploy independently, so
// the only way this repository can notice that it has grown past what Cloud
// tolerates is to assert against the number Cloud actually uses. If Cloud
// changes its horizon, this constant and the assertions below change with it.
const cloudMemberAdoptionSettlementHorizon = 45 * time.Minute

// A confirm that is still retrying here is a join Cloud cannot yet attribute to
// a member. If this worker can keep retrying past Cloud's adoption horizon, then
// Cloud may adopt that member while its hold is also counted, turning one person
// into two seats — and a workspace that owes one seat would be asked to buy two.
//
// Cloud refuses to enforce while such a hold is outstanding, so exceeding the
// horizon is not a correctness break on its own; it stretches the window where
// reconciliation reports itself incomplete and enforcement stays closed. Keep
// the budget inside the horizon so that window closes on its own.
func TestSettlementBudgetStaysInsideCloudAdoptionHorizon(t *testing.T) {
	budget := DefaultSettlementBudget()

	if budget >= cloudMemberAdoptionSettlementHorizon {
		t.Fatalf("settlement budget %s must stay below Multica Cloud's adoption horizon %s;"+
			" lower maxAttempts/backoff here or raise the horizon there first",
			budget, cloudMemberAdoptionSettlementHorizon)
	}

	// Pin the arithmetic so a change to any input is visible in review rather
	// than only in a threshold comparison: nine backoff intervals of
	// 5s+10s+20s+40s+80s+160s+300s+300s+300s = 1215s, plus a 30s tick and 30s of
	// lock wait for each of those nine attempts.
	const wantBackoff = 1215 * time.Second
	const wantOverhead = 9 * (30 * time.Second) * 2
	if want := wantBackoff + wantOverhead; budget != want {
		t.Fatalf("settlement budget = %s, want %s (backoff %s + per-attempt overhead %s)",
			budget, want, wantBackoff, wantOverhead)
	}
}

func TestRetryBackoffIsExponentialAndCapped(t *testing.T) {
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for attempt, expected := range want {
		if got := retryBackoff(int32(attempt)); got != expected {
			t.Fatalf("retryBackoff(%d) = %s, want %s", attempt, got, expected)
		}
	}
}

// A deployment that raises the retry ceiling must fail this contract rather than
// silently widening the window in which Cloud cannot quantify a ledger.
func TestSettlementBudgetGrowsWithConfiguredAttempts(t *testing.T) {
	generous := SettlementBudget(40, defaultReconcileInterval, defaultWorkspaceLockWait)
	if generous < cloudMemberAdoptionSettlementHorizon {
		t.Fatalf("budget for 40 attempts = %s, expected it to exceed the horizon %s so the"+
			" contract is what bounds configuration, not luck", generous, cloudMemberAdoptionSettlementHorizon)
	}
}
