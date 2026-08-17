export function hasAuthoritativeWorkspaceList<T>(
  workspaces: readonly T[] | undefined,
): workspaces is readonly T[] {
  return workspaces !== undefined;
}

export function shouldShowWorkspaceListRecovery<T>({
  authenticated,
  workspaces,
  failed,
}: {
  authenticated: boolean;
  workspaces: readonly T[] | undefined;
  failed: boolean;
}): boolean {
  return authenticated && workspaces === undefined && failed;
}
