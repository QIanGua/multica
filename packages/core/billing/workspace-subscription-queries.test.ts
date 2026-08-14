import { describe, expect, it } from "vitest";
import {
  workspaceSubscriptionPricesOptions,
  workspaceSubscriptionPricesStaleTime,
} from "./workspace-subscription-queries";

describe("workspace subscription price query", () => {
  it("keeps missing price responses stale so the UI can recover", () => {
    expect(workspaceSubscriptionPricesStaleTime(undefined)).toBe(0);
    expect(workspaceSubscriptionPricesStaleTime(null)).toBe(0);
    expect(
      workspaceSubscriptionPricesStaleTime({
        month: {
          currency: "usd",
          unitAmount: 1_000,
          interval: "month",
          intervalCount: 1,
        },
        year: {
          currency: "usd",
          unitAmount: 9_600,
          interval: "year",
          intervalCount: 1,
        },
      }),
    ).toBe(10 * 60 * 1_000);
  });

  it("revalidates stale prices when the Billing window regains focus", () => {
    const options = workspaceSubscriptionPricesOptions("workspace-1");

    expect(options.refetchOnWindowFocus).toBe(true);
    expect(options.retry).toBe(false);
  });
});
