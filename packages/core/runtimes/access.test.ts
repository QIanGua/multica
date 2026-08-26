import { describe, expect, it } from "vitest";
import type { AgentRuntime } from "../types";
import {
  isRuntimeUsableForAgentOwner,
  isRuntimeUsableForUser,
} from "./access";

const OWNER = "user-owner";
const OTHER = "user-other";

function makeRuntime(overrides: Partial<AgentRuntime> = {}): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: "daemon-1",
    name: "Test Runtime",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: OWNER,
    visibility: "private",
    last_seen_at: null,
    created_at: "2026-04-01T00:00:00Z",
    updated_at: "2026-04-01T00:00:00Z",
    ...overrides,
  };
}

// This truth table is the client half of a contract the server enforces with
// the identical predicate (canUseRuntimeForAgent in
// server/internal/handler/runtime.go). Keep the two in step: a runtime this
// says is usable but the server refuses is a picker that 403s on submit —
// the MUL-6126 bug — and the reverse is a runtime the UI hides for no reason.
describe("isRuntimeUsableForUser", () => {
  it("lets the owner use their own private runtime", () => {
    expect(isRuntimeUsableForUser(makeRuntime(), OWNER)).toBe(true);
  });

  it("refuses another member's private runtime", () => {
    expect(isRuntimeUsableForUser(makeRuntime(), OTHER)).toBe(false);
  });

  it("lets anyone use a public runtime", () => {
    expect(
      isRuntimeUsableForUser(makeRuntime({ visibility: "public" }), OTHER),
    ).toBe(true);
  });

  it("refuses an ownerless runtime whatever its visibility", () => {
    // No owner means no task token at claim time (MUL-3292), so a bind here
    // could only produce agents that fail the moment they run.
    expect(isRuntimeUsableForUser(makeRuntime({ owner_id: null }), OTHER)).toBe(
      false,
    );
    expect(
      isRuntimeUsableForUser(
        makeRuntime({ owner_id: null, visibility: "public" }),
        OTHER,
      ),
    ).toBe(false);
  });

  it("allows everything while the viewer is unknown, so a loading session never hides the user's own runtime", () => {
    expect(isRuntimeUsableForUser(makeRuntime(), null)).toBe(true);
  });
});

// The agent-owner half of the same contract (MUL-6704). The server checks it in
// service.RuntimeAllowsAgentOwner, in the SQL claim fence and again after the
// claim, so a picker that offered a target this rejects would bind an agent
// whose every task is then refused — the silent-queue bug the teardown exists
// to end.
describe("isRuntimeUsableForAgentOwner", () => {
  it("lets a private runtime run its own owner's agent", () => {
    expect(isRuntimeUsableForAgentOwner(makeRuntime(), OWNER)).toBe(true);
  });

  it("refuses a private runtime for another member's agent", () => {
    // This is the case that differs from isRuntimeUsableForUser: the runtime
    // owner IS allowed to operate here, but the machine may not run the other
    // member's agent.
    expect(isRuntimeUsableForAgentOwner(makeRuntime(), OTHER)).toBe(false);
  });

  it("lets a public runtime run any owner's agent", () => {
    expect(
      isRuntimeUsableForAgentOwner(makeRuntime({ visibility: "public" }), OTHER),
    ).toBe(true);
  });

  it("refuses an ownerless runtime whatever its visibility", () => {
    expect(
      isRuntimeUsableForAgentOwner(makeRuntime({ owner_id: null }), OWNER),
    ).toBe(false);
  });

  it("allows everything while the agent owner is unknown", () => {
    expect(isRuntimeUsableForAgentOwner(makeRuntime(), null)).toBe(true);
  });
});
