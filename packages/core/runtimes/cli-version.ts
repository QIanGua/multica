/**
 * Frontend mirror of the server's MinQuickCreateCLIVersion gate. The
 * agent-create flow (Quick Create modal) requires the daemon's bundled
 * multica CLI to be at least this version — older daemons either
 * double-create issues on partial CLI failures, drop quick-create attachment
 * bindings, or mishandle pasted screenshot URLs (see PR #1851 / MUL-1496).
 *
 * Both the frontend pre-validation in the modal and the server's
 * `/api/issues/quick-create` handler enforce this; the server is the
 * authoritative trust boundary, the frontend just lets us tell the user
 * "your daemon needs an upgrade" before they hit submit.
 */
export const MIN_QUICK_CREATE_CLI_VERSION = "0.2.21";
export const MIN_QUICK_CREATE_FIELDS_CLI_VERSION = "0.4.3";

export type CliVersionState = "ok" | "too_old" | "missing";

export interface CliVersionCheck {
  state: CliVersionState;
  /** What the daemon reported, or empty if missing/unparsable. */
  current: string;
  /** The hard minimum we gate on. */
  min: string;
}

const SEMVER_RE = /v?(\d+)\.(\d+)\.(\d+)/;

// Matches the `git describe --tags --always --dirty` output for a build past
// the latest tag, e.g. `v0.2.15-235-gdaf0e935` or `v0.2.15-235-gdaf0e935-dirty`.
// Daemons built from source (Makefile `make build` / `make daemon`) report this
// shape; tagged releases are bare semver. Treating dev-described daemons as OK
// is what keeps `pnpm dev:desktop` + `make daemon` unblocked without weakening
// the gate for staging or production users running stale stable releases.
const DEV_DESCRIBE_RE = /^v?\d+\.\d+\.\d+-\d+-g[0-9a-fA-F]+/;

function parseSemver(raw: string): [number, number, number] | null {
  const m = SEMVER_RE.exec(raw.trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

function lessThan(a: [number, number, number], b: [number, number, number]) {
  if (a[0] !== b[0]) return a[0] < b[0];
  if (a[1] !== b[1]) return a[1] < b[1];
  return a[2] < b[2];
}

/**
 * Check a daemon-reported CLI version string against the minimum. Returns
 * `"missing"` for empty/unparsable input (fail closed — same policy as the
 * server) and `"too_old"` for a parsable version below the threshold.
 * Dev-built daemons (git-describe shape) are always OK — the version string
 * itself is the shared signal, so frontend and server agree by construction.
 */
export function checkQuickCreateCliVersion(detected: string | undefined | null): CliVersionCheck {
  return checkCliVersion(detected, MIN_QUICK_CREATE_CLI_VERSION);
}

/** Capability gate for explicit quick-create priority and due-date fields. */
export function checkQuickCreateFieldsCliVersion(
  detected: string | undefined | null,
): CliVersionCheck {
  return checkCliVersion(detected, MIN_QUICK_CREATE_FIELDS_CLI_VERSION);
}

function checkCliVersion(
  detected: string | undefined | null,
  minimum: string,
): CliVersionCheck {
  const current = (detected ?? "").trim();
  if (DEV_DESCRIBE_RE.test(current)) {
    return { state: "ok", current, min: minimum };
  }
  const parsed = current ? parseSemver(current) : null;
  if (!parsed) {
    return { state: "missing", current, min: minimum };
  }
  const min = parseSemver(minimum)!;
  if (lessThan(parsed, min)) {
    return { state: "too_old", current, min: minimum };
  }
  return { state: "ok", current, min: minimum };
}

/** Pull `cli_version` off a runtime row's loosely-typed metadata bag. */
export function readRuntimeCliVersion(metadata: Record<string, unknown> | undefined): string {
  const v = metadata?.cli_version;
  return typeof v === "string" ? v : "";
}

/**
 * Frontend mirror of the server's `MinHandoffCLIVersion` soft gate
 * (`server/pkg/agent/version.go`). The assignment handoff note is only rendered
 * into the run's opening prompt by daemons at or above this multica CLI version
 * (MUL-3375); older daemons silently drop it. Unlike the quick-create gate this
 * never blocks the assignment — the UI just grays out the note box and warns.
 *
 * Keep in lockstep with the server constant; the two are enforced independently
 * (the server is authoritative) but must agree so the warning matches reality.
 */
export const MIN_HANDOFF_CLI_VERSION = "0.3.28";

/**
 * Whether a daemon-reported CLI version is new enough to render a handoff note.
 * Mirrors server `agent.HandoffSupported`: missing / unparsable / below-minimum
 * all degrade to `false`, and dev-built daemons (git-describe shape) always
 * pass — the version string is the shared signal, so frontend and server agree
 * by construction. Pure and synchronous, so the note box can settle from the
 * already-warm runtime cache instead of waiting on the trigger-preview
 * round-trip, exactly like the quick-create version gate.
 */
export function handoffSupported(detected: string | undefined | null): boolean {
  return meetsMinCliVersion(detected, MIN_HANDOFF_CLI_VERSION);
}

/**
 * First release whose daemon renders the chat session's project context
 * (description + resources) into the run brief (PR #5765, ships in v0.4.10).
 * Older daemons still receive and honor the project's repos — the server
 * pre-extracts those into the generic `repos` claim field — but silently skip
 * the Project Context section, so the durable description never reaches the
 * agent. SOFT gate: selecting a project always works; the UI only warns.
 *
 * Frontend-only constant: unlike handoff there is no server preview endpoint
 * computing this, so there is no server twin to keep in lockstep with.
 */
export const MIN_CHAT_PROJECT_CONTEXT_CLI_VERSION = "0.4.10";

/**
 * Whether a daemon-reported CLI version is new enough to inject a chat
 * session's project description into the run brief. Same degrade rules as
 * `handoffSupported`: missing / unparsable / below-minimum are `false`,
 * dev-built daemons (git-describe shape) always pass.
 */
export function chatProjectContextSupported(detected: string | undefined | null): boolean {
  return meetsMinCliVersion(detected, MIN_CHAT_PROJECT_CONTEXT_CLI_VERSION);
}

function meetsMinCliVersion(detected: string | undefined | null, minimum: string): boolean {
  const current = (detected ?? "").trim();
  if (!current) return false;
  if (DEV_DESCRIBE_RE.test(current)) return true;
  const parsed = parseSemver(current);
  if (!parsed) return false;
  return !lessThan(parsed, parseSemver(minimum)!);
}

/**
 * Capability a daemon advertises when it implements worktree mode for
 * local_directory resources. Mirrors `DaemonCapabilityLocalWorktreeV1` in
 * `server/pkg/protocol/messages.go`.
 *
 * This is the whole signal. There is deliberately no version floor left on
 * this side: a dev build reports a git-describe string that `meetsMinCliVersion`
 * exempts, so a daemon with no worktree code at all used to pass a version
 * check (MUL-5707).
 */
export const LOCAL_WORKTREE_CAPABILITY = "local-worktree-v1";

/**
 * First server release that records `X-Client-Capabilities` onto the runtime
 * row it registers (PR #6904, ships in v0.4.25). Every older server stores
 * runtime metadata with no `capabilities` key at all, so through one of them no
 * daemon can be seen to support worktree mode, however new it is.
 *
 * Self-hosted deployments are where this bites: Desktop ships its own renderer
 * AND its own daemon binary and updates both together, so the client routinely
 * runs ahead of a backend the operator upgrades by hand (#7113).
 */
export const MIN_CAPABILITY_AWARE_SERVER_VERSION = "0.4.25";

/**
 * Whether the server the client is TALKING TO records daemon capabilities.
 *
 * Asks the running backend (`/api/config` → `server_version`), never a stored
 * runtime row. A row without capabilities only proves the server was old WHEN
 * IT WAS WRITTEN: upgrading the backend does not rewrite existing metadata and
 * heartbeats touch only `last_seen_at`, so an upgraded deployment would go on
 * being called stale, and re-upgrading it would fix nothing.
 *
 * Deliberately three-valued. An absent version has three real sources and they
 * do not share a remedy:
 *
 * - the managed cloud, which omits the field on purpose (MUL-4108),
 * - an unstamped dev build,
 * - a self-hosted server older than v0.4.2, which is where the field shipped.
 *
 * The first two record capabilities; the third predates the handshake and does
 * not. Collapsing them to "aware" tells that operator to restart the machine,
 * which re-registers against the same blind server and produces the same row —
 * a loop with no exit. Collapsing them to "blind" tells cloud users to upgrade
 * a backend they do not run. So `unknown` stays its own answer and the caller
 * resolves it from evidence.
 */
export type ServerCapabilityAwareness = "aware" | "blind" | "unknown";

export function serverCapabilityAwareness(
  detected: string | undefined | null,
): ServerCapabilityAwareness {
  const current = (detected ?? "").trim();
  if (!current) return "unknown";
  if (DEV_DESCRIBE_RE.test(current)) return "aware";
  const parsed = parseSemver(current);
  if (!parsed) return "unknown";
  return lessThan(parsed, parseSemver(MIN_CAPABILITY_AWARE_SERVER_VERSION)!) ? "blind" : "aware";
}

/**
 * Second opinion for an unnamed server: does ANY runtime row in this workspace
 * carry a `capabilities` key?
 *
 * One does not appear unless a capability-aware server wrote it, and servers
 * move forward — so a single such row anywhere proves the deployment learned
 * the handshake, whoever the row belongs to. That resolves the common shape of
 * `unknown` (managed cloud, several machines, one of them registered before the
 * handshake shipped) into the precise answer without a version to read.
 *
 * The converse is not proof: every row missing the key is what an old
 * self-hosted server looks like, and also what a brand-new workspace with one
 * stale machine looks like. That case stays `unknown` and says so.
 */
function anyRuntimeRecordsCapabilities(runtimes: RuntimeCapabilityRow[]): boolean {
  return runtimes.some(
    (rt) => !!rt.metadata && typeof rt.metadata === "object" && "capabilities" in rt.metadata,
  );
}

/**
 * Whether a machine can run worktree mode, and if not, whose problem it is.
 *
 * Every negative fails closed — the mode exists to keep agents out of the
 * user's working copy, so "just try it" is never the fallback — but each is
 * fixed somewhere else, and a single boolean forced the UI to blame the machine
 * for all of them. That is how a user on the newest release was told to upgrade
 * the very machine that was already newest (#7113).
 *
 * - `daemon_unsupported`: the row records what this daemon can do, and worktree
 *   is not in it. Update Multica on that machine.
 * - `server_capability_blind`: the backend in front of us predates the
 *   handshake, so no daemon can be seen through it. Upgrade the backend.
 * - `runtime_registration_stale`: the backend records capabilities, but this
 *   row was written before it learned to. Nothing is wrong with either half —
 *   the machine just has to register again.
 * - `capability_source_unknown`: the backend does not name a version and no row
 *   anywhere proves it records capabilities. Either remedy might apply, so the
 *   copy names both rather than sending the user down one that cannot work.
 */
export type LocalWorktreeSupport =
  | "supported"
  | "daemon_unsupported"
  | "server_capability_blind"
  | "runtime_registration_stale"
  | "capability_source_unknown";

/**
 * Read one runtime row's answer.
 *
 * The `capabilities` key is written by the server, always — `null` when the
 * daemon sent no header — so its ABSENCE says something about the writer, not
 * about the daemon. Reading a missing key as "this daemon lacks the feature" is
 * the misdiagnosis this type exists to prevent; `serverRecordsCapabilities`
 * then decides whether that writer is still what we are talking to.
 */
function runtimeWorktreeSupport(
  metadata: unknown,
  awareness: ServerCapabilityAwareness,
): LocalWorktreeSupport {
  if (!metadata || typeof metadata !== "object" || !("capabilities" in metadata)) {
    if (awareness === "aware") return "runtime_registration_stale";
    if (awareness === "blind") return "server_capability_blind";
    return "capability_source_unknown";
  }
  const caps = (metadata as { capabilities?: unknown }).capabilities;
  return Array.isArray(caps) && caps.includes(LOCAL_WORKTREE_CAPABILITY)
    ? "supported"
    : "daemon_unsupported";
}

/** Minimal runtime shape this module needs; keeps callers from importing types. */
type RuntimeCapabilityRow = {
  daemon_id?: string | null;
  last_seen_at?: string | null;
  metadata?: unknown;
};

/**
 * Whether the machine behind `daemonId` currently runs a daemon that supports
 * worktree mode, judged by its MOST RECENTLY SEEN runtime row.
 *
 * Deliberately not "any row advertised it". Deregistering a runtime only marks
 * the row offline — its metadata survives — so a machine that once ran a
 * capable daemon and then downgraded still has an old capable row beside the
 * fresh incapable one, and an any-match would answer yes forever. The server's
 * `daemonAdvertisesWorktree` uses the same newest-wins rule; the two must agree
 * or the UI offers a mode the API will refuse.
 *
 * A daemon with no runtime row at all reads as `daemon_unsupported`: nothing
 * there can run the task, and "update Multica on that machine" is the remedy.
 *
 * `serverVersion` is `/api/config`'s `server_version` for the backend this
 * client is connected to — see `serverCapabilityAwareness` for why the question
 * has to be asked of the live server rather than inferred from the row, and why
 * an unnamed server is not assumed to be either kind.
 */
export function localWorktreeSupport(
  runtimes: RuntimeCapabilityRow[],
  daemonId: string | null | undefined,
  serverVersion: string | undefined | null,
): LocalWorktreeSupport {
  if (!daemonId) return "daemon_unsupported";
  let newest: RuntimeCapabilityRow | undefined;
  for (const rt of runtimes) {
    if (rt.daemon_id !== daemonId) continue;
    if (!newest) {
      newest = rt;
      continue;
    }
    // A row that never reported sorts oldest, so a live row always wins.
    const candidateSeen = rt.last_seen_at ?? "";
    const currentSeen = newest.last_seen_at ?? "";
    if (candidateSeen > currentSeen) newest = rt;
  }
  if (!newest) return "daemon_unsupported";
  let awareness = serverCapabilityAwareness(serverVersion);
  // An unnamed server can still be pinned down by what it has written for other
  // machines; nothing can promote it the other way, so only "aware" is derived.
  if (awareness === "unknown" && anyRuntimeRecordsCapabilities(runtimes)) {
    awareness = "aware";
  }
  return runtimeWorktreeSupport(newest.metadata, awareness);
}
