import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { useWorkspaceId } from "../hooks";
import { compareIssueStatusEntries, issueStatusKeys } from "./queries";
import type {
  CreateIssueStatusRequest,
  IssueStatusCategory,
  IssueStatusEntry,
  ListIssueStatusesResponse,
  UpdateIssueStatusRequest,
} from "../types";

/**
 * Catalog mutations (MUL-6243).
 *
 * Every write here installs what the server returned — the row, or for reorder
 * the whole list — and does NOT invalidate the catalog on success. The refresh
 * is the `issue_status:changed` realtime event, which reaches the writing tab
 * as well as every other one, so a catalog edit costs each client exactly ONE
 * catalog read. Invalidating here too would make the admin who did the writing
 * the only client that reads it twice. (MUL-6458)
 *
 * On FAILURE the invalidate stays, and it is not symmetry for its own sake: a
 * 409 usually means this client's catalog is the stale thing — someone else
 * took the name, or archived the row that was being dragged.
 */

function useCatalogCache() {
  const qc = useQueryClient();
  const wsId = useWorkspaceId();
  const listKey = issueStatusKeys.list(wsId);
  return {
    qc,
    listKey,
    /**
     * Writes one authoritative entry into the cached catalog, inserting it when
     * this client has not seen it before.
     *
     * Always re-sorted: `position` and `category` decide where a row renders,
     * so a create (or a position change) that appended to the end would leave
     * the list in an order the server does not agree with until the next fetch.
     */
    putEntry: (entry: IssueStatusEntry) => {
      // A response that failed schema validation degrades to an empty stub
      // (`parseWithFallback`). Writing it would put a blank row in the picker;
      // leaving the cache alone lets the realtime refetch supply the truth.
      if (!entry?.id) return;
      qc.setQueryData<ListIssueStatusesResponse>(listKey, (old) => {
        if (!old) return old;
        const known = old.statuses.some((s) => s.id === entry.id);
        const statuses = known
          ? old.statuses.map((s) => (s.id === entry.id ? entry : s))
          : [...old.statuses, entry];
        return {
          ...old,
          statuses: statuses.sort(compareIssueStatusEntries),
          total: known ? old.total : old.total + 1,
        };
      });
    },
    invalidate: () => {
      qc.invalidateQueries({ queryKey: issueStatusKeys.all(wsId) });
    },
  };
}

export function useCreateIssueStatus() {
  const { putEntry, invalidate } = useCatalogCache();
  return useMutation({
    mutationFn: (data: CreateIssueStatusRequest) => api.createIssueStatus(data),
    onSuccess: putEntry,
    onError: invalidate,
  });
}

/**
 * Optimistic rename / recolor / reorder — the same shape as `useUpdateLabel`.
 * Without it, dragging a row to reorder would snap back for the round-trip.
 */
export function useUpdateIssueStatus() {
  const { qc, listKey, putEntry, invalidate } = useCatalogCache();
  return useMutation({
    mutationFn: ({ id, ...data }: { id: string } & UpdateIssueStatusRequest) =>
      api.updateIssueStatus(id, data),
    onMutate: async ({ id, ...data }) => {
      await qc.cancelQueries({ queryKey: listKey });
      const previous = qc.getQueryData<ListIssueStatusesResponse>(listKey);
      qc.setQueryData<ListIssueStatusesResponse>(listKey, (old) =>
        old
          ? {
              ...old,
              statuses: old.statuses.map((s) => (s.id === id ? { ...s, ...data } : s)),
            }
          : old,
      );
      return { previous };
    },
    // A rename or recolor deliberately does NOT invalidate the issues scope.
    // An issue row stores the status KEY; its name and color are resolved from
    // this catalog at render time (`useStatusLabel`, `colorOf`), so no cached
    // issue field can go stale here — refreshing the catalog is what repaints
    // the boards. The old invalidate refetched every board, list and table in
    // the workspace to change one word. (MUL-6458)
    onSuccess: putEntry,
    onError: (_err, _vars, ctx) => {
      if (ctx?.previous) qc.setQueryData(listKey, ctx.previous);
      invalidate();
    },
  });
}

/**
 * Archives a custom status. Deliberately NOT optimistic: the server refuses to
 * archive a built-in, and a row silently vanishing before that refusal arrives
 * would read as success.
 */
export function useArchiveIssueStatus() {
  const { putEntry, invalidate } = useCatalogCache();
  return useMutation({
    mutationFn: (id: string) => api.archiveIssueStatus(id),
    // The archived row is kept in the cache, not dropped: issues still sitting
    // on it resolve their name, color and category through it. `activeStatuses`
    // is what hides it from pickers.
    onSuccess: putEntry,
    onError: invalidate,
  });
}

/**
 * Commits a drag-reorder within ONE category.
 *
 * Sent as a single request, not a PATCH per row: a sequence of writes is not
 * atomic, so a row rejected part-way (an archived status, a concurrent archive)
 * would leave the rows before it already reordered while the caller is told the
 * whole operation failed. `ordered` is that category's ACTIVE custom statuses;
 * the server assigns positions from 1 because the category's built-in is seeded
 * at 0 and never moves.
 */
export function useReorderIssueStatuses() {
  const { qc, listKey, invalidate } = useCatalogCache();
  return useMutation({
    mutationFn: ({
      category,
      ordered,
    }: {
      category: IssueStatusCategory;
      ordered: IssueStatusEntry[];
    }) => api.reorderIssueStatuses(category, ordered.map((entry) => entry.id)),
    onMutate: async ({ ordered }) => {
      await qc.cancelQueries({ queryKey: listKey });
      const previous = qc.getQueryData<ListIssueStatusesResponse>(listKey);
      const positionById = new Map(ordered.map((e, index) => [e.id, index + 1]));
      qc.setQueryData<ListIssueStatusesResponse>(listKey, (old) =>
        old
          ? {
              ...old,
              // Re-sorted, not just re-positioned: consumers render the array in
              // order, so writing positions alone would leave the drag visually
              // undone until the refetch lands.
              statuses: old.statuses
                .map((s) =>
                  positionById.has(s.id) ? { ...s, position: positionById.get(s.id)! } : s,
                )
                .sort(compareIssueStatusEntries),
            }
          : old,
      );
      return { previous };
    },
    // Reorder answers with the FULL catalog, already in the server's order, so
    // the whole response replaces the optimistic list rather than being merged
    // row by row.
    onSuccess: (response) => {
      // A workspace always has its 7 built-ins, so an empty list is never a
      // real catalog — it is the schema fallback. Keeping the optimistic order
      // and letting the realtime refetch settle it beats blanking the picker.
      if (response.statuses.length === 0) return;
      qc.setQueryData<ListIssueStatusesResponse>(listKey, response);
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.previous) qc.setQueryData(listKey, ctx.previous);
      invalidate();
    },
  });
}
