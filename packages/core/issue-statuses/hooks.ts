import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { buildIssueStatusCatalog, issueStatusListOptions, type IssueStatusCatalog } from "./queries";

/**
 * The workspace status catalog, resolved and memoized (MUL-6243).
 *
 * Takes `wsId` explicitly rather than reading it from context, per the repo's
 * state rules — the catalog is per-workspace, and switching workspaces does not
 * remount the app, so a module-level snapshot would go stale.
 */
export function useIssueStatuses(wsId: string): IssueStatusCatalog {
  const { data, isPending, isError } = useQuery({
    ...issueStatusListOptions(wsId),
    enabled: Boolean(wsId),
  });
  // pending/error are carried through, not dropped: a surface that routes a
  // CUSTOM status key cannot distinguish "catalog not here yet" from "catalog
  // failed" without them, and both need different UI — a spinner and a retry.
  // (MUL-6243)
  return useMemo(
    () => buildIssueStatusCatalog(data, { isPending, isError }),
    [data, isPending, isError],
  );
}
