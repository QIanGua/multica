/**
 * @vitest-environment jsdom
 */
import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { setApiInstance } from "../api";
import type { ApiClient } from "../api/client";
import { issueKeys } from "../issues/queries";
import type { IssueStatusEntry, ListIssueStatusesResponse } from "../types";
import {
  useArchiveIssueStatus,
  useCreateIssueStatus,
  useReorderIssueStatuses,
  useUpdateIssueStatus,
} from "./mutations";
import { issueStatusKeys } from "./queries";

vi.mock("../hooks", () => ({ useWorkspaceId: () => "ws-1" }));

const CATEGORIES = [
  "backlog",
  "todo",
  "in_progress",
  "in_review",
  "done",
  "cancelled",
  "blocked",
] as const;

function entry(overrides: Partial<IssueStatusEntry> & { id: string }): IssueStatusEntry {
  return {
    workspace_id: "ws-1",
    key: overrides.id,
    name: overrides.id,
    description: "",
    category: "in_review",
    color: "#6366f1",
    is_system: false,
    position: 1,
    archived_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

const builtInReview = entry({
  id: "builtin-in-review",
  key: "in_review",
  name: "In Review",
  is_system: true,
  position: 0,
});

function catalog(statuses: IssueStatusEntry[]): ListIssueStatusesResponse {
  return { statuses, categories: [...CATEGORIES], total: statuses.length };
}

function wrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

function createClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } });
}

function cached(qc: QueryClient) {
  return qc.getQueryData<ListIssueStatusesResponse>(issueStatusKeys.list("ws-1"));
}

describe("issue status catalog mutations", () => {
  afterEach(() => vi.restoreAllMocks());

  // The realtime `issue_status:changed` event refreshes this catalog in every
  // tab, the writing one included. A second invalidate here would make the
  // admin who did the writing the only client that reads the catalog twice.
  // (MUL-6458)
  it("leaves the catalog refresh to the realtime event on a successful write", async () => {
    const qc = createClient();
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview]));
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    setApiInstance({
      createIssueStatus: vi.fn(async () => entry({ id: "qa", key: "qa", name: "QA" })),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useCreateIssueStatus(), { wrapper: wrapper(qc) });
    act(() => result.current.mutate({ name: "QA", category: "in_review", color: "#6366f1" }));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    const catalogInvalidations = invalidate.mock.calls.filter(
      ([filters]) => (filters?.queryKey as string[] | undefined)?.[0] === "issue-statuses",
    );
    expect(catalogInvalidations).toHaveLength(0);
  });

  it("still refetches the catalog when a write fails", async () => {
    const qc = createClient();
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview]));
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    setApiInstance({
      createIssueStatus: vi.fn(async () => {
        throw new Error("409");
      }),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useCreateIssueStatus(), { wrapper: wrapper(qc) });
    act(() => result.current.mutate({ name: "QA", category: "in_review", color: "#6366f1" }));
    await waitFor(() => expect(result.current.isError).toBe(true));

    expect(invalidate).toHaveBeenCalledWith({ queryKey: issueStatusKeys.all("ws-1") });
  });

  // Without the sort, a created status lands at the end of the array while the
  // server puts it inside its category — and nothing corrects that until the
  // realtime refetch lands, which is exactly the window the user is looking at.
  it("sorts a created status into its category instead of appending it", async () => {
    const qc = createClient();
    const done = entry({ id: "builtin-done", key: "done", category: "done", is_system: true, position: 0 });
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview, done]));
    setApiInstance({
      createIssueStatus: vi.fn(async () => entry({ id: "qa", key: "qa", name: "QA" })),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useCreateIssueStatus(), { wrapper: wrapper(qc) });
    act(() => result.current.mutate({ name: "QA", category: "in_review", color: "#6366f1" }));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(cached(qc)?.statuses.map((s) => s.id)).toEqual(["builtin-in-review", "qa", "builtin-done"]);
    expect(cached(qc)?.total).toBe(3);
  });

  it("installs the server's row after a rename instead of keeping the optimistic one", async () => {
    const qc = createClient();
    const qa = entry({ id: "qa", key: "qa", name: "QA" });
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview, qa]));
    setApiInstance({
      updateIssueStatus: vi.fn(async () => ({
        ...qa,
        name: "Quality Gate",
        updated_at: "2026-02-02T00:00:00Z",
      })),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useUpdateIssueStatus(), { wrapper: wrapper(qc) });
    act(() => result.current.mutate({ id: "qa", name: "Quality Gate" }));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    const stored = cached(qc)?.statuses.find((s) => s.id === "qa");
    expect(stored?.name).toBe("Quality Gate");
    // The optimistic patch cannot know this — only the response carries it.
    expect(stored?.updated_at).toBe("2026-02-02T00:00:00Z");
  });

  // A rename changes a label the boards resolve from THIS catalog at render
  // time, so nothing cached under the issues scope can be stale. Refetching it
  // meant one word cost a workspace-wide board/list/table refetch. (MUL-6458)
  it("does not refetch the issue caches when a status is renamed", async () => {
    const qc = createClient();
    const qa = entry({ id: "qa", key: "qa", name: "QA" });
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview, qa]));
    const invalidate = vi.spyOn(qc, "invalidateQueries");
    setApiInstance({
      updateIssueStatus: vi.fn(async () => ({ ...qa, name: "Quality Gate" })),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useUpdateIssueStatus(), { wrapper: wrapper(qc) });
    act(() => result.current.mutate({ id: "qa", name: "Quality Gate" }));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: issueKeys.all("ws-1") });
  });

  // An archived status stays in the cache on purpose: issues still sitting on
  // it resolve their name, color and category through it.
  it("keeps an archived status in the catalog with its archived_at set", async () => {
    const qc = createClient();
    const qa = entry({ id: "qa", key: "qa", name: "QA" });
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview, qa]));
    setApiInstance({
      archiveIssueStatus: vi.fn(async () => ({ ...qa, archived_at: "2026-02-02T00:00:00Z" })),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useArchiveIssueStatus(), { wrapper: wrapper(qc) });
    act(() => result.current.mutate("qa"));
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(cached(qc)?.statuses.find((s) => s.id === "qa")?.archived_at).toBe("2026-02-02T00:00:00Z");
    expect(cached(qc)?.total).toBe(2);
  });

  it("replaces the whole catalog with the ordered list a reorder returns", async () => {
    const qc = createClient();
    const first = entry({ id: "qa", key: "qa", position: 1 });
    const second = entry({ id: "sec", key: "sec", position: 2 });
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview, first, second]));
    setApiInstance({
      reorderIssueStatuses: vi.fn(async () =>
        catalog([builtInReview, { ...second, position: 1 }, { ...first, position: 2 }]),
      ),
    } as unknown as ApiClient);

    const { result } = renderHook(() => useReorderIssueStatuses(), { wrapper: wrapper(qc) });
    act(() =>
      result.current.mutate({ category: "in_review", ordered: [second, first] }),
    );
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(cached(qc)?.statuses.map((s) => s.id)).toEqual(["builtin-in-review", "sec", "qa"]);
  });

  // `parseWithFallback` degrades a malformed response to an empty stub. Writing
  // that would blank the picker until the realtime refetch lands.
  it("ignores a response that degraded to the empty schema fallback", async () => {
    const qc = createClient();
    const qa = entry({ id: "qa", key: "qa", name: "QA" });
    qc.setQueryData(issueStatusKeys.list("ws-1"), catalog([builtInReview, qa]));
    setApiInstance({
      updateIssueStatus: vi.fn(async () => entry({ id: "", key: "", name: "", position: 0 })),
      reorderIssueStatuses: vi.fn(async () => ({ statuses: [], categories: [], total: 0 })),
    } as unknown as ApiClient);

    const update = renderHook(() => useUpdateIssueStatus(), { wrapper: wrapper(qc) });
    act(() => update.result.current.mutate({ id: "qa", name: "Quality Gate" }));
    await waitFor(() => expect(update.result.current.isSuccess).toBe(true));
    expect(cached(qc)?.statuses.map((s) => s.id)).toEqual(["builtin-in-review", "qa"]);

    const reorder = renderHook(() => useReorderIssueStatuses(), { wrapper: wrapper(qc) });
    act(() => reorder.result.current.mutate({ category: "in_review", ordered: [qa] }));
    await waitFor(() => expect(reorder.result.current.isSuccess).toBe(true));
    expect(cached(qc)?.statuses).toHaveLength(2);
  });
});
