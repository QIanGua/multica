import { create } from "zustand";
import type { InboxItem, IssuePriority, IssueStatus } from "../types";

export interface InboxFilters {
  readonly statuses: readonly IssueStatus[];
  readonly priorities: readonly IssuePriority[];
  /** Actor keys — see `inboxActorKey`. */
  readonly actors: readonly string[];
  readonly unreadOnly: boolean;
}

/**
 * Stable identity for "who this notification came from", used as the value of
 * the `actors` dimension.
 *
 * Members and agents key on their id. `system` has no id — the backend writes
 * an invalid UUID that serializes to null — so it keys on the type alone and
 * every system notification collapses into one bucket. Returns null when the
 * row carries no usable attribution; such a row can never match an actor
 * selection, exactly as a row without an issue can never match a status one.
 */
export function inboxActorKey(item: InboxItem): string | null {
  const type = item.actor_type;
  if (!type) return null;
  if (type === "system") return "system";
  return item.actor_id ? `${type}:${item.actor_id}` : null;
}

/** Inverse of `inboxActorKey`, for resolving a selection back to a directory. */
export function inboxActorKeyParts(key: string): { type: string; id: string } {
  const separator = key.indexOf(":");
  if (separator === -1) return { type: key, id: "" };
  return { type: key.slice(0, separator), id: key.slice(separator + 1) };
}

/**
 * Identity of the per-issue group the list renders as ONE row. Shared with
 * the deduplication in `./queries` so grouping and filtering can never drift.
 */
export function inboxGroupKey(item: InboxItem): string {
  return item.issue_id ?? item.id;
}

/**
 * Every actor that appears anywhere in a group, keyed by group.
 *
 * Status and priority are properties of the issue, so reading them off the
 * one row that survives deduplication loses nothing. An actor is a property
 * of the individual NOTIFICATION, and only the newest one survives — so
 * matching the surviving row alone would hide an issue Alice commented on
 * yesterday the moment Bob touches it today. The index restores the rest of
 * the group.
 *
 * Pass the same rows the matching deduplication consumed (`inboxActorIndex` /
 * `archivedInboxActorIndex` in `./queries` apply the same archived filter);
 * an archived row indexed against the active list would contribute an actor
 * to a group the active list does not show.
 */
export type InboxActorIndex = ReadonlyMap<string, ReadonlySet<string>>;

export const EMPTY_INBOX_ACTOR_INDEX: InboxActorIndex = new Map();

const NO_ACTORS: ReadonlySet<string> = new Set();

export function buildInboxActorIndex(
  items: readonly InboxItem[],
): InboxActorIndex {
  const index = new Map<string, Set<string>>();
  for (const item of items) {
    const actor = inboxActorKey(item);
    if (!actor) continue;
    const key = inboxGroupKey(item);
    const group = index.get(key);
    if (group) group.add(actor);
    else index.set(key, new Set([actor]));
  }
  return index;
}

/**
 * Actors behind the row's whole group. Falls back to the row's own actor when
 * the group is absent from the index, so a caller holding a single row still
 * filters on something truthful rather than on nothing.
 */
export function inboxGroupActorKeys(
  item: InboxItem,
  index: InboxActorIndex,
): ReadonlySet<string> {
  const group = index.get(inboxGroupKey(item));
  if (group) return group;
  const own = inboxActorKey(item);
  return own ? new Set([own]) : NO_ACTORS;
}

export type InboxPriorityFilterSupport =
  | "unknown"
  | "supported"
  | "unsupported";

export const EMPTY_INBOX_FILTERS: InboxFilters = Object.freeze({
  statuses: Object.freeze([]),
  priorities: Object.freeze([]),
  actors: Object.freeze([]),
  unreadOnly: false,
});

/** True when nothing is selected in any dimension. */
export function isEmptyInboxFilters(filters: InboxFilters): boolean {
  return inboxFilterCount(filters) === 0;
}

interface InboxFilterState {
  filtersByWorkspace: Record<string, InboxFilters>;
  toggleStatusFilter: (wsId: string, status: IssueStatus) => void;
  togglePriorityFilter: (wsId: string, priority: IssuePriority) => void;
  toggleActorFilter: (wsId: string, actor: string) => void;
  toggleUnreadOnly: (wsId: string) => void;
  clearPriorityFilters: (wsId: string) => void;
  clearFilters: (wsId: string) => void;
}

function toggleValue<T extends string>(values: readonly T[], value: T): T[] {
  return values.includes(value)
    ? values.filter((candidate) => candidate !== value)
    : [...values, value];
}

export const useInboxFilterStore = create<InboxFilterState>()((set) => ({
  filtersByWorkspace: {},
  toggleStatusFilter: (wsId, status) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: {
            ...current,
            statuses: toggleValue(current.statuses, status),
          },
        },
      };
    }),
  togglePriorityFilter: (wsId, priority) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: {
            ...current,
            priorities: toggleValue(current.priorities, priority),
          },
        },
      };
    }),
  toggleActorFilter: (wsId, actor) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, actors: toggleValue(current.actors, actor) },
        },
      };
    }),
  toggleUnreadOnly: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId] ?? EMPTY_INBOX_FILTERS;
      return {
        filtersByWorkspace: {
          ...state.filtersByWorkspace,
          [wsId]: { ...current, unreadOnly: !current.unreadOnly },
        },
      };
    }),
  clearPriorityFilters: (wsId) =>
    set((state) => {
      const current = state.filtersByWorkspace[wsId];
      if (!current || current.priorities.length === 0) return state;
      const next = { ...current, priorities: [] };
      // Emptiness is asked of every dimension, not just statuses: dropping the
      // entry while an actor or unread selection survived in it would clear a
      // filter the user set and the backend never had a say in.
      if (isEmptyInboxFilters(next)) {
        const { [wsId]: _removed, ...filtersByWorkspace } =
          state.filtersByWorkspace;
        return { filtersByWorkspace };
      }
      return {
        filtersByWorkspace: { ...state.filtersByWorkspace, [wsId]: next },
      };
    }),
  clearFilters: (wsId) =>
    set((state) => {
      if (!state.filtersByWorkspace[wsId]) return state;
      const { [wsId]: _removed, ...filtersByWorkspace } =
        state.filtersByWorkspace;
      return { filtersByWorkspace };
    }),
}));

/** Workspace-isolated filter state with a stable empty fallback. */
export function useInboxFilters(wsId: string): InboxFilters {
  return (
    useInboxFilterStore((state) => state.filtersByWorkspace[wsId]) ??
    EMPTY_INBOX_FILTERS
  );
}

/**
 * Capability inferred from the parsed response, not from a version string.
 *
 * The new backend always serializes `issue_priority` for every Inbox row
 * (including `null` for notifications without an issue). An older backend
 * omits it. Requiring every returned row to carry a defined value also keeps a
 * priority cache patch from making a mixed rolling-deploy response look fully
 * supported.
 */
export function inboxPriorityFilterSupport(
  items: readonly InboxItem[],
): InboxPriorityFilterSupport {
  if (items.length === 0) return "unknown";
  return items.every((item) => item.issue_priority !== undefined)
    ? "supported"
    : "unsupported";
}

/** Ignore a stale priority selection until the backend capability is proven. */
export function inboxFiltersForPrioritySupport(
  filters: InboxFilters,
  support: InboxPriorityFilterSupport,
): InboxFilters {
  if (support === "supported" || filters.priorities.length === 0) {
    return filters;
  }
  return { ...filters, priorities: [] };
}

function matchesAnyActor(
  groupActors: ReadonlySet<string>,
  selected: ReadonlySet<string>,
): boolean {
  for (const actor of groupActors) {
    if (selected.has(actor)) return true;
  }
  return false;
}

/**
 * OR within a dimension, AND between dimensions.
 *
 * `unreadOnly` reads the surviving row's own `read`, which is what the unread
 * badge counts (`useInboxUnreadCount`) — so "only unread" and the number next
 * to Inbox can never disagree. The actor dimension reads the whole group; see
 * `InboxActorIndex`.
 */
export function filterInboxItems(
  items: InboxItem[],
  filters: InboxFilters,
  actorIndex: InboxActorIndex,
): InboxItem[] {
  if (isEmptyInboxFilters(filters)) return items;

  const statuses = new Set(filters.statuses);
  const priorities = new Set(filters.priorities);
  const actors = new Set(filters.actors);
  return items.filter(
    (item) =>
      (statuses.size === 0 ||
        (item.issue_status != null && statuses.has(item.issue_status))) &&
      (priorities.size === 0 ||
        (item.issue_priority != null && priorities.has(item.issue_priority))) &&
      (actors.size === 0 ||
        matchesAnyActor(inboxGroupActorKeys(item, actorIndex), actors)) &&
      (!filters.unreadOnly || item.read !== true),
  );
}

export function inboxFilterCount(filters: InboxFilters): number {
  return (
    filters.statuses.length +
    filters.priorities.length +
    filters.actors.length +
    (filters.unreadOnly ? 1 : 0)
  );
}
