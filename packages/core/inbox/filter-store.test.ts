// @vitest-environment node

import { beforeEach, describe, expect, it } from "vitest";
import type { InboxItem } from "../types";
import {
  buildInboxActorIndex,
  EMPTY_INBOX_ACTOR_INDEX,
  EMPTY_INBOX_FILTERS,
  filterInboxItems,
  inboxActorKey,
  inboxFiltersForPrioritySupport,
  inboxFilterCount,
  inboxPriorityFilterSupport,
  useInboxFilterStore,
} from "./filter-store";

function item(
  id: string,
  issueStatus: InboxItem["issue_status"],
  issuePriority: InboxItem["issue_priority"],
  overrides: Partial<InboxItem> = {},
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
    ...overrides,
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
    expect(
      filterInboxItems(ITEMS, EMPTY_INBOX_FILTERS, EMPTY_INBOX_ACTOR_INDEX),
    ).toBe(ITEMS);
  });

  it("uses OR within a dimension and AND between dimensions", () => {
    expect(
      filterInboxItems(
        ITEMS,
        { ...EMPTY_INBOX_FILTERS, statuses: ["todo", "done"], priorities: ["high"] },
        EMPTY_INBOX_ACTOR_INDEX,
      ).map((candidate) => candidate.id),
    ).toEqual(["todo-high", "done-high"]);
  });

  it("excludes notifications without an issue when an issue filter is active", () => {
    expect(
      filterInboxItems(
        ITEMS,
        { ...EMPTY_INBOX_FILTERS, priorities: ["high"] },
        EMPTY_INBOX_ACTOR_INDEX,
      ).map((candidate) => candidate.id),
    ).toEqual(["todo-high", "done-high"]);
  });

  it("does not apply a priority condition until the projection is supported", () => {
    const filters = {
      ...EMPTY_INBOX_FILTERS,
      statuses: ["todo"],
      priorities: ["high"],
      actors: ["member:alice"],
      unreadOnly: true,
    } as const;

    // Only the unsupported dimension is dropped — a backend that cannot
    // project priority still has nothing to say about who sent a
    // notification or whether it was read.
    expect(inboxFiltersForPrioritySupport(filters, "unknown")).toEqual({
      ...filters,
      priorities: [],
    });
    expect(inboxFiltersForPrioritySupport(filters, "unsupported")).toEqual({
      ...filters,
      priorities: [],
    });
    expect(inboxFiltersForPrioritySupport(filters, "supported")).toBe(filters);
  });
});

describe("actor filtering", () => {
  const alice = { actor_type: "member" as const, actor_id: "alice" };
  const bob = { actor_type: "agent" as const, actor_id: "bob" };

  it("keys members and agents on their id and collapses system into one bucket", () => {
    expect(inboxActorKey(item("a", "todo", "high", alice))).toBe("member:alice");
    expect(inboxActorKey(item("b", "todo", "high", bob))).toBe("agent:bob");
    expect(
      inboxActorKey(
        item("c", "todo", "high", { actor_type: "system", actor_id: null }),
      ),
    ).toBe("system");
    expect(inboxActorKey(item("d", "todo", "high"))).toBeNull();
  });

  it("matches an actor anywhere in the group, not just the surviving row", () => {
    // Alice commented, then Bob changed the status. Deduplication keeps Bob's
    // row; filtering by Alice must still surface the issue.
    const raw = [
      item("newest", "todo", "high", { issue_id: "issue-1", ...bob }),
      item("older", "todo", "high", { issue_id: "issue-1", ...alice }),
    ];
    const index = buildInboxActorIndex(raw);
    const deduplicated = [raw[0]!];

    expect(
      filterInboxItems(
        deduplicated,
        { ...EMPTY_INBOX_FILTERS, actors: ["member:alice"] },
        index,
      ).map((candidate) => candidate.id),
    ).toEqual(["newest"]);
    expect(
      filterInboxItems(
        deduplicated,
        { ...EMPTY_INBOX_FILTERS, actors: ["member:carol"] },
        index,
      ),
    ).toEqual([]);
  });

  it("falls back to the row's own actor when its group is missing from the index", () => {
    const row = item("solo", "todo", "high", alice);

    expect(
      filterInboxItems(
        [row],
        { ...EMPTY_INBOX_FILTERS, actors: ["member:alice"] },
        EMPTY_INBOX_ACTOR_INDEX,
      ),
    ).toEqual([row]);
  });

  it("excludes a row that carries no attribution", () => {
    const row = item("unattributed", "todo", "high");

    expect(
      filterInboxItems(
        [row],
        { ...EMPTY_INBOX_FILTERS, actors: ["member:alice"] },
        buildInboxActorIndex([row]),
      ),
    ).toEqual([]);
  });
});

describe("unread filtering", () => {
  it("keeps unread rows and counts as one active filter", () => {
    const rows = [
      item("unread", "todo", "high"),
      item("read", "todo", "high", { read: true }),
    ];
    const filters = { ...EMPTY_INBOX_FILTERS, unreadOnly: true };

    expect(
      filterInboxItems(rows, filters, EMPTY_INBOX_ACTOR_INDEX).map(
        (candidate) => candidate.id,
      ),
    ).toEqual(["unread"]);
    expect(inboxFilterCount(filters)).toBe(1);
  });

  it("keeps notifications without an issue, unlike the issue dimensions", () => {
    // The issue-less rows are the quick-create failures — the ones that most
    // need to stay reachable while screening for what is unhandled.
    const rows = [item("no-issue", null, null)];

    expect(
      filterInboxItems(
        rows,
        { ...EMPTY_INBOX_FILTERS, unreadOnly: true },
        EMPTY_INBOX_ACTOR_INDEX,
      ),
    ).toEqual(rows);
  });
});

describe("inboxPriorityFilterSupport", () => {
  it("distinguishes an omitted legacy projection from a supported null", () => {
    expect(inboxPriorityFilterSupport([])).toBe("unknown");
    expect(inboxPriorityFilterSupport([item("system", null, null)])).toBe(
      "supported",
    );
    expect(inboxPriorityFilterSupport([item("legacy", "todo", undefined)])).toBe(
      "unsupported",
    );
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
      useInboxFilterStore.getState().filtersByWorkspace["ws-1"]?.unreadOnly,
    ).toBe(false);
    expect(
      inboxFilterCount(useInboxFilterStore.getState().filtersByWorkspace["ws-1"]!),
    ).toBe(1);

    useInboxFilterStore.getState().clearFilters("ws-1");
    expect(useInboxFilterStore.getState().filtersByWorkspace["ws-1"]).toBeUndefined();
    expect(useInboxFilterStore.getState().filtersByWorkspace["ws-2"]).toBeDefined();
  });

  it("clears only the unsupported priority dimension", () => {
    const store = useInboxFilterStore.getState();
    store.toggleStatusFilter("ws-1", "todo");
    store.togglePriorityFilter("ws-1", "high");

    useInboxFilterStore.getState().clearPriorityFilters("ws-1");

    expect(useInboxFilterStore.getState().filtersByWorkspace["ws-1"]).toEqual({
      ...EMPTY_INBOX_FILTERS,
      statuses: ["todo"],
    });
  });

  it("keeps an actor or unread selection alive when priority is dropped", () => {
    const store = useInboxFilterStore.getState();
    store.toggleActorFilter("ws-1", "member:alice");
    store.toggleUnreadOnly("ws-1");
    store.togglePriorityFilter("ws-1", "high");

    useInboxFilterStore.getState().clearPriorityFilters("ws-1");

    expect(useInboxFilterStore.getState().filtersByWorkspace["ws-1"]).toEqual({
      ...EMPTY_INBOX_FILTERS,
      actors: ["member:alice"],
      unreadOnly: true,
    });
  });

  it("drops the workspace entry when clearing priority leaves nothing", () => {
    useInboxFilterStore.getState().togglePriorityFilter("ws-1", "high");

    useInboxFilterStore.getState().clearPriorityFilters("ws-1");

    expect(
      useInboxFilterStore.getState().filtersByWorkspace["ws-1"],
    ).toBeUndefined();
  });

  it("toggles the unread dimension off again", () => {
    useInboxFilterStore.getState().toggleUnreadOnly("ws-1");
    expect(
      useInboxFilterStore.getState().filtersByWorkspace["ws-1"]?.unreadOnly,
    ).toBe(true);

    useInboxFilterStore.getState().toggleUnreadOnly("ws-1");
    expect(
      useInboxFilterStore.getState().filtersByWorkspace["ws-1"]?.unreadOnly,
    ).toBe(false);
  });
});
