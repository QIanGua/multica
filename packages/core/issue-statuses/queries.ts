import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import { STATUS_CONFIG, STATUS_ORDER } from "../issues/config";
import type { IssueStatusCategory, IssueStatusEntry } from "../types";

/**
 * The workspace issue status catalog (MUL-6243).
 *
 * A workspace always has the 7 built-in statuses and may define custom ones.
 * Every status — built-in or custom — belongs to exactly one of the 7
 * CATEGORIES, and the category is what determines platform behavior, board
 * column and presentation. So this catalog answers one question for the UI:
 * given a status key stored on an issue, what is its label, color and category?
 */

export const issueStatusKeys = {
  all: (wsId: string) => ["issue-statuses", wsId] as const,
  list: (wsId: string) => [...issueStatusKeys.all(wsId), "list"] as const,
};

export function issueStatusListOptions(wsId: string) {
  return queryOptions({
    queryKey: issueStatusKeys.list(wsId),
    queryFn: () => api.listIssueStatuses(),
    select: (data) => data.statuses,
    // The catalog changes only when an admin edits it, which is rare, so a
    // generous stale time keeps this off the critical path of every render
    // that needs a status label.
    staleTime: 5 * 60_000,
  });
}

/**
 * A resolved view over the catalog. Every lookup falls back to something
 * renderable, because an issue can legitimately carry a status this client has
 * not heard of: a status created moments ago in another session, or one whose
 * catalog fetch has not landed yet.
 */
export interface IssueStatusCatalog {
  /** Active statuses in display order (category order, then position). */
  statuses: IssueStatusEntry[];
  /** Category for a status key; falls back to the key when it is a built-in, else "todo". */
  categoryOf: (statusKey: string) => IssueStatusCategory;
  /** Human label for a status key; falls back to the category label, then the raw key. */
  labelOf: (statusKey: string) => string;
  /** Catalog entry for a status key, when the catalog knows it. */
  entryOf: (statusKey: string) => IssueStatusEntry | undefined;
  /** Statuses belonging to one category, in display order. */
  inCategory: (category: IssueStatusCategory) => IssueStatusEntry[];
  /** True once the catalog has loaded; false while it is still in flight. */
  isLoaded: boolean;
}

const BUILT_IN = new Set<string>(STATUS_ORDER);

export function isIssueStatusCategory(value: string): value is IssueStatusCategory {
  return BUILT_IN.has(value);
}

/**
 * Builds the resolved catalog from a raw entry list. Pure, so the store and
 * other non-React callers can use it with a list they already hold.
 */
export function buildIssueStatusCatalog(
  entries: IssueStatusEntry[] | undefined,
): IssueStatusCatalog {
  const list = entries ?? [];
  const byKey = new Map(list.map((e) => [e.key, e]));

  const categoryOf = (statusKey: string): IssueStatusCategory => {
    const category = byKey.get(statusKey)?.category;
    if (category && isIssueStatusCategory(category)) return category;
    // A built-in key IS its own category, so an unloaded catalog still resolves
    // all 7 correctly — which is what keeps the default workspace rendering
    // identically before the fetch lands.
    if (isIssueStatusCategory(statusKey)) return statusKey;
    // An unknown custom key: render it somewhere sane rather than dropping it.
    return "todo";
  };

  return {
    statuses: list,
    categoryOf,
    entryOf: (statusKey) => byKey.get(statusKey),
    labelOf: (statusKey) => {
      const entry = byKey.get(statusKey);
      if (entry) return entry.name;
      if (isIssueStatusCategory(statusKey)) return STATUS_CONFIG[statusKey]?.label ?? statusKey;
      return statusKey;
    },
    inCategory: (category) => list.filter((e) => e.category === category),
    isLoaded: entries !== undefined,
  };
}
