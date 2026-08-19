import type {
  AutopilotQuotaUsage,
  WorkspaceSubscriptionEntitlements,
  WorkspaceSubscriptionSummary,
} from "@multica/core/types";

export type AutopilotUsageView =
  | { kind: "unlimited" }
  | { kind: "unavailable" }
  | {
      kind: "metered";
      used: number;
      reserved: number;
      total: number;
      limit: number;
      progress: number;
      reached: boolean;
      resetAt: string;
    };

/**
 * Quota admission counts completed and reserved runs. Keep reserved work
 * visible so the progress bar matches the server's blocking decision.
 */
export function resolveAutopilotUsage(
  entitlements: WorkspaceSubscriptionEntitlements,
  usage: AutopilotQuotaUsage | undefined,
  failed: boolean,
): AutopilotUsageView {
  if (
    entitlements.plan === "pro" &&
    entitlements.autopilotRuns === null
  ) {
    return { kind: "unlimited" };
  }

  if (
    failed ||
    !usage ||
    usage.action === "off" ||
    usage.used === null ||
    usage.reserved === null ||
    usage.limit === null ||
    usage.reset_at === null ||
    usage.used < 0 ||
    usage.reserved < 0 ||
    usage.limit < 0 ||
    !Number.isFinite(usage.used) ||
    !Number.isFinite(usage.reserved) ||
    !Number.isFinite(usage.limit)
  ) {
    return { kind: "unavailable" };
  }

  const total = usage.used + usage.reserved;
  const reached = total >= usage.limit;
  const progress =
    usage.limit === 0
      ? 100
      : Math.min(100, Math.max(0, (total / usage.limit) * 100));

  return {
    kind: "metered",
    used: usage.used,
    reserved: usage.reserved,
    total,
    limit: usage.limit,
    progress,
    reached,
    resetAt: usage.reset_at,
  };
}

export function isBillingSnapshotExpired(
  expiresAt: string | null,
  now: number = Date.now(),
): boolean {
  if (!expiresAt) return false;
  const expiresAtMs = new Date(expiresAt).getTime();
  return Number.isFinite(expiresAtMs) && expiresAtMs <= now;
}

const MANAGED_SUBSCRIPTION_STATUSES = new Set([
  "active",
  "trialing",
  "past_due",
  "canceled",
  "incomplete",
  "paused",
  "unpaid",
]);

/**
 * Summary facts are primary. Plan and status are compatibility fallbacks so
 * an older or temporarily unavailable summary does not hide the recovery UI.
 */
export function hasManagedWorkspaceSubscription(
  entitlements: WorkspaceSubscriptionEntitlements,
  summary: WorkspaceSubscriptionSummary | null | undefined,
): boolean {
  return (
    summary?.hasStripeCustomer === true ||
    summary?.billedSeats !== null && summary?.billedSeats !== undefined ||
    entitlements.plan === "pro" ||
    MANAGED_SUBSCRIPTION_STATUSES.has(entitlements.status)
  );
}
