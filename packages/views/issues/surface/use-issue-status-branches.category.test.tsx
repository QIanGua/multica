/**
 * @vitest-environment jsdom
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { setApiInstance } from "@multica/core/api";
import type { ApiClient } from "@multica/core/api/client";
import type { Issue, IssueStatusCategory, IssueTableRowsRequest } from "@multica/core/types";
import { useIssueStatusBranches } from "./use-issue-status-branches";

/**
 * Board and list columns are CATEGORIES (MUL-6243). Before this, the hook asked
 * the server for a concrete status key AND re-checked `row.issue.status` against
 * the column key, so a card on a custom status was dropped twice over: it never
 * came back from the server, and it would have been filtered out if it had.
 */

function makeIssue(id: string, status: string, category: IssueStatusCategory): Issue {
  return {
    id,
    workspace_id: "ws-1",
    number: 1,
    identifier: `MUL-${id}`,
    title: id,
    description: null,
    status,
    status_category: category,
    priority: "none",
    assignee_type: null,
    assignee_id: null,
    creator_type: "member",
    creator_id: "u-1",
    project_id: null,
    parent_issue_id: null,
    stage: null,
    position: 1,
    start_date: null,
    due_date: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  } as Issue;
}

const QUERY = {
  scope: { kind: "workspace" as const },
  filters: {},
  sort: { field: "position" as const, direction: "asc" as const },
};

function wrapper(qc: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("useIssueStatusBranches — category columns", () => {
  it("pages each column by category and keeps custom-status rows in it", async () => {
    const requests: IssueTableRowsRequest[] = [];
    const listIssueTableRows = vi.fn(async (request: IssueTableRowsRequest) => {
      requests.push(request);
      const rows =
        request.group_key === "status_category:in_review"
          ? [
              { issue: makeIssue("qa-1", "qa", "in_review"), direct_child_count: 0 },
              { issue: makeIssue("std-1", "in_review", "in_review"), direct_child_count: 0 },
            ]
          : [];
      return {
        query_fingerprint: "test",
        group_key: request.group_key ?? null,
        parent_id: null,
        total: rows.length,
        rows,
        branch_total: rows.length,
        next_cursor: null,
      };
    });
    setApiInstance({
      listIssueTableRows,
      listIssueStatuses: async () => ({ statuses: [], categories: [], total: 0 }),
    } as unknown as ApiClient);

    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    });
    const { result } = renderHook(
      () =>
        useIssueStatusBranches({
          wsId: "ws-1",
          query: QUERY,
          statuses: ["in_review"],
          facets: undefined,
          facetsPending: false,
          facetsFetching: false,
          enabled: true,
        }),
      { wrapper: wrapper(qc) },
    );

    await waitFor(() => expect(result.current.issues.length).toBe(2));

    // The request names the CATEGORY contract, not a concrete status key.
    expect(requests[0]?.group).toEqual({ kind: "status_category" });
    expect(requests[0]?.group_key).toBe("status_category:in_review");
    // Both rows land in the column: the custom one is not dropped for failing
    // to equal the column key.
    expect(result.current.issues.map((i) => i.id).sort()).toEqual(["qa-1", "std-1"]);
  });

  it("folds a custom status's facet count into its category's column total", async () => {
    setApiInstance({
      listIssueTableRows: async (request: IssueTableRowsRequest) => ({
        query_fingerprint: "test",
        group_key: request.group_key ?? null,
        parent_id: null,
        total: 0,
        rows: [],
        branch_total: 0,
        next_cursor: null,
      }),
      listIssueStatuses: async () => ({
        statuses: [
          {
            id: "s-qa",
            workspace_id: "ws-1",
            key: "qa",
            name: "QA",
            description: "",
            category: "in_review",
            color: "#ff0000",
            is_system: false,
            position: 1,
            archived_at: null,
            created_at: "",
            updated_at: "",
          },
        ],
        categories: [],
        total: 1,
      }),
    } as unknown as ApiClient);

    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    });
    const { result } = renderHook(
      () =>
        useIssueStatusBranches({
          wsId: "ws-1",
          query: QUERY,
          statuses: ["in_review"],
          facets: {
            query_fingerprint: "test",
            total: 5,
            facets: [
              {
                kind: "status",
                // The facet is keyed by concrete status KEY. Discarding the
                // custom key made the column header disagree with its cards.
                values: [
                  { key: "in_review", count: 2 },
                  { key: "qa", count: 3 },
                ],
              },
            ],
          },
          facetsPending: false,
          facetsFetching: false,
          enabled: true,
        }),
      { wrapper: wrapper(qc) },
    );

    await waitFor(() =>
      expect(result.current.pagination.in_review?.total).toBe(5),
    );
  });
});
