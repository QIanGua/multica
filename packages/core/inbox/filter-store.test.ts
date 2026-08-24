// @vitest-environment node

import { beforeEach, describe, expect, it } from "vitest";
import type { InboxItem } from "../types";
import {
  EMPTY_INBOX_FILTERS,
  filterInboxItems,
  inboxFilterCount,
  useInboxFilterStore,
} from "./filter-store";

function item(
  id: string,
  issueStatus: InboxItem["issue_status"],
  issuePriority: InboxItem["issue_priority"],
): InboxItem {
  return {
    id,
    workspace_id: "ws-1",
    recipient_type: "member",
    recipient_id: "member-1",
    actor_type: null,
    actor_id: null,
    type: "mentioned",
    severity: "info",
    issue_id: issueStatus == null ? null : `issue-${id}`,
    title: id,
    body: null,
    issue_status: issueStatus,
    issue_priority: issuePriority,
    read: false,
    archived: false,
    created_at: "2026-08-24T00:00:00Z",
    details: null,
  };
}

const ITEMS = [
  item("todo-high", "todo", "high"),
  item("todo-low", "todo", "low"),
  item("done-high", "done", "high"),
  item("system", null, null),
];

beforeEach(() => {
  useInboxFilterStore.setState({ filtersByWorkspace: {} });
});

describe("filterInboxItems", () => {
  it("keeps the original reference when no filter is active", () => {
    expect(filterInboxItems(ITEMS, EMPTY_INBOX_FILTERS)).toBe(ITEMS);
  });

  it("uses OR within a dimension and AND between dimensions", () => {
    expect(
      filterInboxItems(ITEMS, {
        statuses: ["todo", "done"],
        priorities: ["high"],
      }).map((candidate) => candidate.id),
    ).toEqual(["todo-high", "done-high"]);
  });

  it("excludes notifications without an issue when an issue filter is active", () => {
    expect(
      filterInboxItems(ITEMS, { statuses: [], priorities: ["high"] }).map(
        (candidate) => candidate.id,
      ),
    ).toEqual(["todo-high", "done-high"]);
  });
});

describe("useInboxFilterStore", () => {
  it("keeps filters isolated by workspace and clears one workspace only", () => {
    const store = useInboxFilterStore.getState();
    store.toggleStatusFilter("ws-1", "todo");
    store.togglePriorityFilter("ws-2", "urgent");

    expect(useInboxFilterStore.getState().filtersByWorkspace).toMatchObject({
      "ws-1": { statuses: ["todo"], priorities: [] },
      "ws-2": { statuses: [], priorities: ["urgent"] },
    });
    expect(
      inboxFilterCount(useInboxFilterStore.getState().filtersByWorkspace["ws-1"]!),
    ).toBe(1);

    useInboxFilterStore.getState().clearFilters("ws-1");
    expect(useInboxFilterStore.getState().filtersByWorkspace["ws-1"]).toBeUndefined();
    expect(useInboxFilterStore.getState().filtersByWorkspace["ws-2"]).toBeDefined();
  });
});
