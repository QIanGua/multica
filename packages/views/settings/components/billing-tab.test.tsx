import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@multica/core/api";
import { configStore } from "@multica/core/config";
import { BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG } from "@multica/core/feature-flags";
import { renderWithI18n } from "../../test/i18n";

const mocks = vi.hoisted(() => ({
  useQuery: vi.fn(),
  checkout: vi.fn(),
  portal: vi.fn(),
  reconcile: vi.fn(),
  refetch: vi.fn(),
  openExternal: vi.fn(),
  role: "owner" as "owner" | "admin" | "member",
  entitlements: {
    workspaceId: "workspace-1",
    plan: "free",
    status: "inactive",
    seats: 3,
    issueWindow: 1000,
    autopilotRuns: 100,
    currentPeriodEnd: null as string | null,
    snapshotExpiresAt: null as string | null,
    version: 0,
  },
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: unknown) => mocks.useQuery(options),
}));

vi.mock("@multica/core/billing", () => ({
  workspaceSubscriptionEntitlementsOptions: (wsId: string) => ({
    queryKey: ["workspace-subscriptions", wsId, "entitlements"],
  }),
  useCreateWorkspaceSubscriptionCheckout: () => ({
    mutateAsync: mocks.checkout,
    isPending: false,
  }),
  useCreateWorkspaceSubscriptionPortal: () => ({
    mutateAsync: mocks.portal,
    isPending: false,
  }),
  useReconcileWorkspaceSubscriptionSeats: () => ({
    mutateAsync: mocks.reconcile,
    isPending: false,
  }),
}));

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({
    id: "workspace-1",
    slug: "acme",
    name: "Acme",
  }),
}));

vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: mocks.role, isLoading: false }),
}));

const navigationState = {
  search: "tab=billing",
  replace: vi.fn(),
};
vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    pathname: "/acme/settings",
    searchParams: new URLSearchParams(navigationState.search),
    push: vi.fn(),
    replace: navigationState.replace,
    back: vi.fn(),
    getShareableUrl: vi.fn(),
  }),
}));

vi.mock("../../platform", () => ({ openExternal: mocks.openExternal }));

import { BillingTab } from "./billing-tab";

describe("BillingTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    navigationState.search = "tab=billing";
    mocks.role = "owner";
    Object.assign(mocks.entitlements, {
      plan: "free",
      status: "inactive",
      seats: 3,
      issueWindow: 1000,
      autopilotRuns: 100,
      currentPeriodEnd: null,
      snapshotExpiresAt: null,
      version: 0,
    });
    mocks.useQuery.mockReturnValue({
      data: mocks.entitlements,
      isPending: false,
      isError: false,
      refetch: mocks.refetch,
    });
    mocks.checkout.mockResolvedValue({
      requestId: "request-1",
      sessionId: "cs_test_1",
      url: "https://checkout.stripe.com/test-session",
    });
    mocks.portal.mockResolvedValue({
      url: "https://billing.stripe.com/test-session",
    });
    mocks.reconcile.mockResolvedValue({
      workspaceId: "workspace-1",
      billedSeats: 3,
      actualSeats: 3,
      action: "none",
    });
    configStore.getState().setFeatureFlags({
      [BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG]: true,
    });
  });

  it("renders nothing and issues no query when the feature flag is absent", () => {
    configStore.getState().setFeatureFlags({});

    const { container } = renderWithI18n(<BillingTab />);

    expect(container).toBeEmptyDOMElement();
    expect(mocks.useQuery).not.toHaveBeenCalled();
  });

  it("shows an honest Free plan without inventing prices or usage totals", () => {
    renderWithI18n(<BillingTab />);

    expect(screen.getByRole("heading", { name: "Billing" })).toBeInTheDocument();
    expect(screen.getByText("1,000")).toBeInTheDocument();
    expect(screen.getByText("100 / month")).toBeInTheDocument();
    expect(
      screen.getByText(/Stripe Checkout shows the authoritative per-seat price/),
    ).toBeInTheDocument();
    expect(screen.queryByText("$10")).not.toBeInTheDocument();
  });

  it("creates Checkout with a client idempotency key and opens Stripe externally", async () => {
    const user = userEvent.setup();
    renderWithI18n(<BillingTab />);

    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Continue to Stripe" }));

    await waitFor(() => {
      expect(mocks.checkout).toHaveBeenCalledTimes(1);
    });
    const request = mocks.checkout.mock.calls[0]?.[0] as {
      interval: string;
      idempotencyKey: string;
    };
    expect(request.interval).toBe("month");
    expect(request.idempotencyKey).toMatch(/^workspace-checkout-workspace-1-/);
    expect(mocks.openExternal).toHaveBeenCalledWith(
      "https://checkout.stripe.com/test-session",
      { webTarget: "same-tab" },
    );
  });

  it("reuses the Checkout idempotency key after an ambiguous failure", async () => {
    const user = userEvent.setup();
    mocks.checkout
      .mockRejectedValueOnce(new Error("network lost after submit"))
      .mockResolvedValueOnce({
        requestId: "request-1",
        sessionId: "cs_test_1",
        url: "https://checkout.stripe.com/test-session",
      });
    renderWithI18n(<BillingTab />);

    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Continue to Stripe" }));
    await screen.findByText("Billing action failed");

    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Continue to Stripe" }));

    await waitFor(() => expect(mocks.checkout).toHaveBeenCalledTimes(2));
    expect(mocks.checkout.mock.calls[1]?.[0].idempotencyKey).toBe(
      mocks.checkout.mock.calls[0]?.[0].idempotencyKey,
    );
  });

  it("starts a new Checkout intent after explicit cancellation", async () => {
    const user = userEvent.setup();
    mocks.checkout
      .mockRejectedValueOnce(new Error("network lost after submit"))
      .mockResolvedValueOnce({
        requestId: "request-2",
        sessionId: "cs_test_2",
        url: "https://checkout.stripe.com/test-session-2",
      });
    renderWithI18n(<BillingTab />);

    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Continue to Stripe" }));
    await screen.findByText("Billing action failed");

    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Continue to Stripe" }));

    await waitFor(() => expect(mocks.checkout).toHaveBeenCalledTimes(2));
    expect(mocks.checkout.mock.calls[1]?.[0].idempotencyKey).not.toBe(
      mocks.checkout.mock.calls[0]?.[0].idempotencyKey,
    );
  });

  it.each([
    [503, "Billing is temporarily unavailable. Retry in a moment."],
    [
      403,
      "Your workspace role no longer allows this action. Refresh the page to see your current access.",
    ],
  ])("maps a %s Checkout response to actionable copy", async (status, copy) => {
    const user = userEvent.setup();
    mocks.checkout.mockRejectedValue(
      new ApiError("request failed", status, "Request Failed"),
    );
    renderWithI18n(<BillingTab />);

    await user.click(screen.getByRole("button", { name: "Upgrade to Pro" }));
    await user.click(screen.getByRole("button", { name: "Continue to Stripe" }));

    expect(await screen.findByText(copy)).toBeInTheDocument();
  });

  it("consumes cancel callback params once while preserving the active tab", () => {
    navigationState.search =
      "tab=billing&result=cancel&session_id=cs_test_1&source=email";

    renderWithI18n(<BillingTab />);

    expect(screen.getByText("Checkout canceled")).toBeInTheDocument();
    expect(navigationState.replace).toHaveBeenCalledOnce();
    expect(navigationState.replace).toHaveBeenCalledWith(
      "/acme/settings?tab=billing&source=email",
    );
  });

  it("polls after a success callback and surfaces a bounded sync timeout", () => {
    vi.useFakeTimers();
    navigationState.search = "tab=billing&result=success&session_id=cs_test_1";
    try {
      renderWithI18n(<BillingTab />);

      expect(
        screen.getByText("Activating your subscription"),
      ).toBeInTheDocument();
      expect(mocks.useQuery).toHaveBeenCalledWith(
        expect.objectContaining({ refetchInterval: 2_000 }),
      );
      expect(navigationState.replace).toHaveBeenCalledWith(
        "/acme/settings?tab=billing",
      );

      act(() => vi.advanceTimersByTime(30_000));

      expect(
        screen.getByText(
          "Payment was received, but the subscription is still syncing. Refresh this page in a moment.",
        ),
      ).toBeInTheDocument();
      expect(navigationState.replace).toHaveBeenCalledOnce();
    } finally {
      vi.useRealTimers();
    }
  });

  it("keeps plan facts visible but hides subscription mutations from members", () => {
    mocks.role = "member";

    renderWithI18n(<BillingTab />);

    expect(screen.getByText("Read-only billing access")).toBeInTheDocument();
    expect(screen.getAllByText("3 members")).toHaveLength(2);
    expect(
      screen.queryByRole("button", { name: "Upgrade to Pro" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Refresh seats" }),
    ).not.toBeInTheDocument();
  });

  it("shows Pro management and unlimited limits without a second Checkout", () => {
    Object.assign(mocks.entitlements, {
      plan: "pro",
      status: "active",
      issueWindow: null,
      autopilotRuns: null,
      currentPeriodEnd: "2026-09-13T00:00:00Z",
      version: 3,
    });

    renderWithI18n(<BillingTab />);

    expect(
      screen.getByRole("button", { name: "Manage billing" }),
    ).toBeInTheDocument();
    expect(screen.getAllByText("Unlimited")).toHaveLength(2);
    expect(
      screen.queryByRole("button", { name: "Upgrade to Pro" }),
    ).not.toBeInTheDocument();
  });

  it("keeps the billing portal available for a canceled Pro period", () => {
    Object.assign(mocks.entitlements, {
      plan: "pro",
      status: "canceled",
      currentPeriodEnd: "2026-09-13T00:00:00Z",
      version: 4,
    });

    renderWithI18n(<BillingTab />);

    expect(
      screen.getByRole("button", { name: "Manage billing" }),
    ).toBeInTheDocument();
  });

  it("keeps payment recovery available when past-due grace has resolved to Free", () => {
    Object.assign(mocks.entitlements, {
      plan: "free",
      status: "past_due",
      snapshotExpiresAt: "2026-09-01T00:00:00Z",
      version: 4,
    });

    renderWithI18n(<BillingTab />);

    expect(screen.getByText("Payment needs attention")).toBeInTheDocument();
    expect(screen.queryByText("Sep 1, 2026")).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Manage billing" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Upgrade to Pro" }),
    ).not.toBeInTheDocument();
  });
});
