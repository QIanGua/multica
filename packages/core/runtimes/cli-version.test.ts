import { describe, it, expect } from "vitest";
import {
  chatProjectContextSupported,
  checkQuickCreateCliVersion,
  checkQuickCreateFieldsCliVersion,
  handoffSupported,
  MIN_CHAT_PROJECT_CONTEXT_CLI_VERSION,
  MIN_HANDOFF_CLI_VERSION,
  localWorktreeSupport,
  serverRecordsDaemonCapabilities,
} from "./cli-version";

describe("checkQuickCreateCliVersion", () => {
  it("returns ok for a tagged release at or above the minimum", () => {
    expect(checkQuickCreateCliVersion("v0.2.21").state).toBe("ok");
    expect(checkQuickCreateCliVersion("0.3.1").state).toBe("ok");
  });

  it("returns too_old for a tagged release below the minimum", () => {
    expect(checkQuickCreateCliVersion("v0.2.20").state).toBe("too_old");
    expect(checkQuickCreateCliVersion("v0.2.15").state).toBe("too_old");
  });

  it("returns missing for empty or unparsable input", () => {
    expect(checkQuickCreateCliVersion("").state).toBe("missing");
    expect(checkQuickCreateCliVersion(undefined).state).toBe("missing");
    expect(checkQuickCreateCliVersion("not-a-version").state).toBe("missing");
  });

  it("treats git-describe dev builds as ok regardless of base tag", () => {
    expect(checkQuickCreateCliVersion("v0.2.15-235-gdaf0e935").state).toBe("ok");
    expect(checkQuickCreateCliVersion("v0.2.15-235-gdaf0e935-dirty").state).toBe("ok");
    expect(checkQuickCreateCliVersion("0.1.0-1-gabc1234").state).toBe("ok");
  });
});

describe("checkQuickCreateFieldsCliVersion", () => {
  it("requires the first daemon release that transports explicit fields", () => {
    expect(checkQuickCreateFieldsCliVersion("0.4.2").state).toBe("too_old");
    expect(checkQuickCreateFieldsCliVersion("0.4.3").state).toBe("ok");
    expect(checkQuickCreateFieldsCliVersion("v0.4.3-1-gabc1234").state).toBe("ok");
  });
});

// Mirrors server/pkg/agent/handoff_version_test.go so the frontend soft-gate
// signal and the server's authoritative one agree by construction.
describe("handoffSupported", () => {
  it("supports a tagged release at or above the minimum", () => {
    expect(handoffSupported(MIN_HANDOFF_CLI_VERSION)).toBe(true);
    expect(handoffSupported("0.4.0")).toBe(true);
    expect(handoffSupported("v0.3.28")).toBe(true);
  });

  it("does not support a tagged release below the minimum", () => {
    expect(handoffSupported("0.3.26")).toBe(false);
    expect(handoffSupported("0.2.21")).toBe(false);
  });

  it("fails closed on empty or unparsable input", () => {
    expect(handoffSupported("")).toBe(false);
    expect(handoffSupported(undefined)).toBe(false);
    expect(handoffSupported(null)).toBe(false);
    expect(handoffSupported("garbage")).toBe(false);
  });

  it("treats git-describe dev builds as supported regardless of base tag", () => {
    expect(handoffSupported("v0.3.0-5-gabc1234")).toBe(true);
    expect(handoffSupported("v0.1.0-235-gdaf0e935-dirty")).toBe(true);
  });
});

describe("chatProjectContextSupported", () => {
  it("supports a tagged release at or above the minimum", () => {
    expect(chatProjectContextSupported(MIN_CHAT_PROJECT_CONTEXT_CLI_VERSION)).toBe(true);
    expect(chatProjectContextSupported("v0.4.10")).toBe(true);
    expect(chatProjectContextSupported("0.5.0")).toBe(true);
  });

  it("does not support a tagged release below the minimum", () => {
    expect(chatProjectContextSupported("0.4.9")).toBe(false);
    expect(chatProjectContextSupported("0.3.28")).toBe(false);
  });

  it("fails closed on empty or unparsable input", () => {
    expect(chatProjectContextSupported("")).toBe(false);
    expect(chatProjectContextSupported(undefined)).toBe(false);
    expect(chatProjectContextSupported(null)).toBe(false);
    expect(chatProjectContextSupported("garbage")).toBe(false);
  });

  it("treats git-describe dev builds as supported regardless of base tag", () => {
    expect(chatProjectContextSupported("v0.4.8-37-g5d0275d68")).toBe(true);
    expect(chatProjectContextSupported("v0.1.0-235-gdaf0e935-dirty")).toBe(true);
  });
});

describe("serverRecordsDaemonCapabilities", () => {
  it("reads the running backend's version", () => {
    expect(serverRecordsDaemonCapabilities("0.4.25")).toBe(true);
    expect(serverRecordsDaemonCapabilities("v0.4.28")).toBe(true);
    expect(serverRecordsDaemonCapabilities("0.4.24")).toBe(false);
    expect(serverRecordsDaemonCapabilities("0.3.0")).toBe(false);
  });

  // Absent is not "old". /api/config omits server_version on the managed cloud
  // by design (MUL-4108) and leaves it empty for unstamped dev builds — both
  // record capabilities — while the field itself shipped in v0.4.2, long before
  // the handshake. Guessing "old" here would tell cloud users to upgrade a
  // backend they do not run.
  it("treats an absent or unreadable version as capability-aware", () => {
    for (const v of ["", "   ", undefined, null, "unknown", "v0.4.21-24-gcd3c0bb89"]) {
      expect(serverRecordsDaemonCapabilities(v)).toBe(true);
    }
  });
});

describe("localWorktreeSupport", () => {
  const CURRENT = "0.4.28";
  const BLIND = "0.4.24";
  const capable = { capabilities: ["skill-bundles-v1", "local-worktree-v1"] };
  // A capability-aware server writes the key even when the daemon sent no
  // header, so `null` is what an old daemon on a current server looks like.
  const incapable = { cli_version: "9.9.9", capabilities: null };
  const row = (metadata: unknown, last_seen_at: string | null = "2026-08-13T00:00:00Z") => ({
    daemon_id: "d1",
    last_seen_at,
    metadata,
  });

  it("reads the advertised capability", () => {
    expect(localWorktreeSupport([row(capable)], "d1", CURRENT)).toBe("supported");
    expect(
      localWorktreeSupport([row({ capabilities: ["skill-bundles-v1"] })], "d1", CURRENT),
    ).toBe("daemon_unsupported");
  });

  // The whole reason this replaced a version check: a dev-built daemon reports a
  // git-describe string that the version floor exempts, so a binary with no
  // worktree implementation passed and two tasks ran in the user's own
  // directory (MUL-5707). The capability must ignore daemon versions entirely.
  it("ignores the daemon version string in both directions", () => {
    expect(
      localWorktreeSupport([row({ cli_version: "9.9.9", capabilities: [] })], "d1", CURRENT),
    ).toBe("daemon_unsupported");
    expect(
      localWorktreeSupport(
        [row({ cli_version: "0.0.1", capabilities: ["local-worktree-v1"] })],
        "d1",
        CURRENT,
      ),
    ).toBe("supported");
  });

  // #7113: a self-hosted backend older than the handshake stores no
  // `capabilities` key at all, so the newest daemon on earth reads as
  // unsupported through it. Blaming the machine sent a user on v0.4.28 off to
  // upgrade past v0.4.24 — a floor they had already cleared.
  it("blames a backend that predates the handshake", () => {
    for (const metadata of [{}, { cli_version: "v0.4.28" }, undefined, null, "nope", 42]) {
      expect(localWorktreeSupport([row(metadata)], "d1", BLIND)).toBe("server_capability_blind");
    }
    // The key present but useless is the daemon's answer, not the server's —
    // even when the server is old enough to be a suspect.
    expect(localWorktreeSupport([row(incapable)], "d1", BLIND)).toBe("daemon_unsupported");
    expect(
      localWorktreeSupport([row({ capabilities: "local-worktree-v1" })], "d1", CURRENT),
    ).toBe("daemon_unsupported");
  });

  // The row only proves the server was old WHEN IT WROTE. Upgrading the backend
  // does not rewrite runtime metadata and heartbeats touch only last_seen_at, so
  // an upgraded deployment would be called stale forever — and upgrading it
  // again would change nothing. That is a different remedy: register again.
  it("does not call an upgraded backend old just because an old row survives", () => {
    expect(localWorktreeSupport([row({ cli_version: "v0.4.28" })], "d1", CURRENT)).toBe(
      "runtime_registration_stale",
    );
    // Same row, backend still behind: now the backend genuinely is the problem.
    expect(localWorktreeSupport([row({ cli_version: "v0.4.28" })], "d1", BLIND)).toBe(
      "server_capability_blind",
    );
    // Managed cloud omits server_version entirely; it must never read as old.
    expect(localWorktreeSupport([row({ cli_version: "v0.4.28" })], "d1", "")).toBe(
      "runtime_registration_stale",
    );
  });

  // Deregistering only marks a runtime offline; its metadata survives and the
  // list endpoint still returns it. An any-row match would therefore keep
  // vouching for a machine that has since downgraded, so the UI would offer a
  // mode the server refuses at claim time.
  it("ignores a stale capable row once a newer row lacks the capability", () => {
    expect(
      localWorktreeSupport(
        [row(capable, "2026-08-01T00:00:00Z"), row(incapable, "2026-08-13T00:00:00Z")],
        "d1",
        CURRENT,
      ),
    ).toBe("daemon_unsupported");
  });

  it("recognises an upgrade: newest row advertises it", () => {
    expect(
      localWorktreeSupport(
        [row(incapable, "2026-08-01T00:00:00Z"), row(capable, "2026-08-13T00:00:00Z")],
        "d1",
        CURRENT,
      ),
    ).toBe("supported");
  });

  it("does not depend on array order", () => {
    const rows = [row(incapable, "2026-08-13T00:00:00Z"), row(capable, "2026-08-01T00:00:00Z")];
    expect(localWorktreeSupport(rows, "d1", CURRENT)).toBe("daemon_unsupported");
    expect(localWorktreeSupport([...rows].reverse(), "d1", CURRENT)).toBe("daemon_unsupported");
  });

  it("a row that never reported loses to one that did", () => {
    expect(
      localWorktreeSupport(
        [row(capable, null), row(incapable, "2026-08-13T00:00:00Z")],
        "d1",
        CURRENT,
      ),
    ).toBe("daemon_unsupported");
  });

  it("ignores other daemons, and fails closed with no rows or no id", () => {
    const other = [{ daemon_id: "d2", last_seen_at: "2026-08-13T00:00:00Z", metadata: capable }];
    expect(localWorktreeSupport(other, "d1", CURRENT)).toBe("daemon_unsupported");
    expect(localWorktreeSupport([], "d1", CURRENT)).toBe("daemon_unsupported");
    expect(localWorktreeSupport(other, null, CURRENT)).toBe("daemon_unsupported");
  });
});
