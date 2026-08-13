import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enBilling from "../locales/en/billing.json";

const mockReplace = vi.hoisted(() => vi.fn());
const searchRef = vi.hoisted(() => ({ current: new URLSearchParams() }));
const authRef = vi.hoisted(() => ({
  current: { user: { id: "user-1" } as { id: string } | null, isLoading: false },
}));
const mockRefetchWorkspaces = vi.hoisted(() => vi.fn());
const workspacesRef = vi.hoisted(() => ({
  current: {
    data: [
      { id: "ws-1", slug: "acme" },
      { id: "ws-2", slug: "globex" },
    ] as { id: string; slug: string }[] | undefined,
    isPending: false,
    isError: false,
    refetch: mockRefetchWorkspaces,
  },
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => workspacesRef.current,
}));

vi.mock("@multica/core/workspace/queries", () => ({
  workspaceListOptions: () => ({ queryKey: ["workspaces"], queryFn: vi.fn() }),
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = Object.assign(
    (selector?: (state: typeof authRef.current) => unknown) =>
      selector ? selector(authRef.current) : authRef.current,
    { getState: () => authRef.current },
  );
  return { useAuthStore };
});

vi.mock("../navigation", () => ({
  useNavigation: () => ({
    replace: mockReplace,
    searchParams: searchRef.current,
    pathname: "/billing/return",
  }),
}));

import { BillingReturnPage } from "./billing-return-page";

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={{ en: { billing: enBilling } }}>
      {children}
    </I18nProvider>
  );
}

function renderReturn(search: string) {
  searchRef.current = new URLSearchParams(search);
  return render(<BillingReturnPage />, { wrapper: Wrapper });
}

describe("BillingReturnPage", () => {
  beforeEach(() => {
    authRef.current = { user: { id: "user-1" }, isLoading: false };
    workspacesRef.current = {
      data: [
        { id: "ws-1", slug: "acme" },
        { id: "ws-2", slug: "globex" },
      ],
      isPending: false,
      isError: false,
      refetch: mockRefetchWorkspaces,
    };
  });

  afterEach(() => {
    mockReplace.mockReset();
    mockRefetchWorkspaces.mockReset();
  });

  it("resolves the workspace id cloud authorized into its slug", async () => {
    renderReturn("workspace_id=ws-2&result=success&session_id=cs_test_123");

    await waitFor(() =>
      expect(mockReplace).toHaveBeenCalledWith(
        "/globex/settings?tab=billing&result=success",
      ),
    );
  });

  it("forwards cancel and portal results and drops the Stripe session id", async () => {
    renderReturn("workspace_id=ws-1&result=cancel&session_id=cs_test_9");
    await waitFor(() =>
      expect(mockReplace).toHaveBeenCalledWith(
        "/acme/settings?tab=billing&result=cancel",
      ),
    );

    mockReplace.mockReset();
    renderReturn("workspace_id=ws-1&result=portal");
    await waitFor(() =>
      expect(mockReplace).toHaveBeenCalledWith(
        "/acme/settings?tab=billing&result=portal",
      ),
    );
  });

  // Only `workspace_id` and `result` are read, and the destination is rebuilt
  // from paths — so a hand-crafted return link cannot redirect off-app.
  it("ignores an unrecognized result instead of forwarding it", async () => {
    renderReturn("workspace_id=ws-1&result=https://evil.example/steal");

    await waitFor(() =>
      expect(mockReplace).toHaveBeenCalledWith("/acme/settings?tab=billing"),
    );
  });

  // Paying and then landing somewhere unexplained is worse than a short
  // message, so an unresolvable workspace is explained rather than redirected.
  it("explains an unresolvable workspace instead of silently redirecting", async () => {
    renderReturn("workspace_id=ws-does-not-exist&result=success");

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "That workspace is no longer available to you",
      ),
    );
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Your billing change was saved",
    );
    expect(mockReplace).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Go to Multica" }));
    expect(mockReplace).toHaveBeenCalledWith("/");
  });

  it("explains a missing workspace id the same way", async () => {
    renderReturn("result=success");

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "That workspace is no longer available to you",
      ),
    );
    expect(mockReplace).not.toHaveBeenCalled();
  });

  // A session can expire while the user is on Stripe, so this is a normal path.
  // Losing the return context here would leave the user on their default
  // workspace with no idea whether the payment landed.
  it("carries the return URL through login when the session expired", async () => {
    authRef.current = { user: null, isLoading: false };
    renderReturn("workspace_id=ws-1&result=success&session_id=cs_test_1");

    await waitFor(() => {
      expect(mockReplace).toHaveBeenCalledWith(
        "/login?next=" +
          encodeURIComponent(
            "/billing/return?workspace_id=ws-1&result=success&session_id=cs_test_1",
          ),
      );
    });
    // Relative path only — the login page's sanitizer rejects anything else, so
    // this cannot become an off-site redirect.
    const target = mockReplace.mock.calls[0]?.[0] as string;
    expect(decodeURIComponent(target.split("next=")[1] ?? "")).toMatch(/^\/billing\/return\?/);
  });

  it("offers a retry instead of spinning forever when the workspace list fails", async () => {
    workspacesRef.current = {
      data: undefined,
      isPending: false,
      isError: true,
      refetch: mockRefetchWorkspaces,
    };
    renderReturn("workspace_id=ws-1&result=success");

    expect(screen.getByRole("alert")).toHaveTextContent(
      "Could not load your workspaces",
    );
    expect(mockReplace).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(mockRefetchWorkspaces).toHaveBeenCalledTimes(1);
  });

  it("waits for auth and the workspace list before deciding", async () => {
    authRef.current = { user: null, isLoading: true };
    renderReturn("workspace_id=ws-1&result=success");
    expect(mockReplace).not.toHaveBeenCalled();

    authRef.current = { user: { id: "user-1" }, isLoading: false };
    workspacesRef.current = {
      data: undefined,
      isPending: true,
      isError: false,
      refetch: mockRefetchWorkspaces,
    };
    renderReturn("workspace_id=ws-1&result=success");
    expect(mockReplace).not.toHaveBeenCalled();
  });

  it("shows a polite status while redirecting", () => {
    renderReturn("workspace_id=ws-1&result=success");

    expect(screen.getByRole("status")).toHaveTextContent(
      "Returning you to workspace billing",
    );
  });
});
