import { describe, expect, it } from "vitest";
import {
  hasAuthoritativeWorkspaceList,
  shouldShowWorkspaceListRecovery,
} from "./workspace-list-gate";

describe("desktop workspace-list gate", () => {
  it("keeps the shell available when a background refetch fails", () => {
    const cached = [{ id: "ws-1", slug: "acme" }];

    expect(hasAuthoritativeWorkspaceList(cached)).toBe(true);
    expect(
      shouldShowWorkspaceListRecovery({
        authenticated: true,
        workspaces: cached,
        failed: true,
      }),
    ).toBe(false);
  });

  it("shows recovery when the initial request fails without data", () => {
    expect(hasAuthoritativeWorkspaceList(undefined)).toBe(false);
    expect(
      shouldShowWorkspaceListRecovery({
        authenticated: true,
        workspaces: undefined,
        failed: true,
      }),
    ).toBe(true);
  });
});
