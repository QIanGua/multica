import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";

export const workspaceSubscriptionKeys = {
  all: (wsId: string) => ["workspace-subscriptions", wsId] as const,
  entitlements: (wsId: string) =>
    [...workspaceSubscriptionKeys.all(wsId), "entitlements"] as const,
};

export function workspaceSubscriptionEntitlementsOptions(wsId: string) {
  return queryOptions({
    queryKey: workspaceSubscriptionKeys.entitlements(wsId),
    queryFn: () => api.getWorkspaceSubscriptionEntitlements(),
    enabled: wsId.length > 0,
    staleTime: 60 * 1000,
    refetchOnWindowFocus: true,
  });
}
