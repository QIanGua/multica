import type { RuntimeDevice } from "../types";

/**
 * Whether this member may run an agent on `runtime`.
 *
 * A private runtime is usable only by its owner; a public one by anyone in the
 * workspace. Workspace role does not enter into it: a private runtime is
 * someone's own machine, so not even an admin may bind an agent to it — they
 * flip it to public first. The server enforces exactly this predicate
 * (`canUseRuntimeForAgent`), so the picker and the API/CLI never disagree
 * (MUL-6126).
 *
 * An unknown viewer (no session yet) is treated as allowed so a still-loading
 * auth state never hides a runtime the user does own — every write path
 * re-checks server-side.
 *
 * This is the single source of truth for "can this runtime be picked": every
 * create / duplicate / builder / reassign surface must call it rather than
 * re-deriving the rule.
 */
export function isRuntimeUsableForUser(
  runtime: RuntimeDevice,
  currentUserId: string | null,
): boolean {
  if (!currentUserId) return true;
  if (runtime.owner_id === currentUserId) return true;
  return runtime.visibility === "public";
}
