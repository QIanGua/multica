// @vitest-environment jsdom

import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import type { Agent, AgentRuntime } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

const mockRevoke = vi.hoisted(() => vi.fn());
const mockToastError = vi.hoisted(() => vi.fn());

// Hoisted so the vi.mock factory (which is itself hoisted) can return it.
const MockApiError = vi.hoisted(
  () =>
    class MockApiError extends Error {
      status: number;
      body: unknown;
      constructor(status: number, body: unknown) {
        super("conflict");
        this.status = status;
        this.body = body;
      }
    },
);

vi.mock("@multica/core/api", () => ({ ApiError: MockApiError }));

vi.mock("sonner", () => ({
  toast: { error: (...args: unknown[]) => mockToastError(...args), success: vi.fn() },
}));

vi.mock("@multica/core/runtimes", () => ({
  runtimeDisplayLabel: (rt: { name: string }) => rt.name,
}));

vi.mock("@multica/core/runtimes/mutations", () => ({
  useRevokeRuntimeAndMakePrivate: () => ({
    mutateAsync: (...args: unknown[]) => mockRevoke(...args),
    isPending: false,
  }),
}));

import {
  RevokeVisibilityDialog,
  parseRuntimeRevokeConflict,
  type RuntimeRevokePlan,
} from "./revoke-visibility-dialog";

function makeRuntime(): AgentRuntime {
  return {
    id: "rt-1",
    workspace_id: "ws-1",
    daemon_id: null,
    name: "Studio Mac",
    runtime_mode: "local",
    provider: "claude",
    launch_header: "",
    status: "online",
    device_info: "",
    metadata: {},
    owner_id: "user-me",
    visibility: "public",
    last_seen_at: null,
    created_at: "2026-04-01T00:00:00Z",
    updated_at: "2026-04-01T00:00:00Z",
  } as AgentRuntime;
}

function makeAgent(id: string, name: string): Agent {
  return { id, name } as Agent;
}

function makePlan(over: Partial<RuntimeRevokePlan> = {}): RuntimeRevokePlan {
  return {
    activeAgents: [makeAgent("agent-1", "Teammate Agent")],
    archivedAgentCount: 0,
    retainedAgentCount: 0,
    mikaAffected: false,
    ...over,
  };
}

function renderDialog(plan: RuntimeRevokePlan, onRevoked = vi.fn()) {
  return {
    onRevoked,
    ...render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <RevokeVisibilityDialog
          open
          onOpenChange={vi.fn()}
          runtime={makeRuntime()}
          wsId="ws-1"
          plan={plan}
          onRevoked={onRevoked}
        />
      </I18nProvider>,
    ),
  };
}

// MUL-6704. Reclaiming a shared machine cancels other people's work and pauses
// their automations, so the dialog's job is to make sure the user approves the
// exact set that will be affected — and re-approves it if that set moves.
describe("RevokeVisibilityDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockRevoke.mockResolvedValue({
      status: "ok",
      agents_unbound: 1,
      tasks_cancelled: 2,
      autopilots_paused: 1,
      agents_retained: 0,
    });
  });

  it("requires the confirmation checkbox before it will submit", async () => {
    renderDialog(makePlan());

    const confirmButton = screen.getByRole("button", {
      name: "Make private",
    }) as HTMLButtonElement;
    expect(confirmButton.disabled).toBe(true);

    fireEvent.click(screen.getByRole("checkbox"));
    expect(confirmButton.disabled).toBe(false);

    fireEvent.click(confirmButton);
    await waitFor(() =>
      expect(mockRevoke).toHaveBeenCalledWith({
        runtimeId: "rt-1",
        // The confirmed snapshot travels with the request: the server compares
        // it under a lock so the user cannot approve plan A and get plan B.
        expectedActiveAgentIds: ["agent-1"],
      }),
    );
  });

  it("names the affected agents and warns about a workspace-wide Mika outage", () => {
    renderDialog(
      makePlan({
        activeAgents: [makeAgent("agent-1", "Mika")],
        mikaAffected: true,
      }),
    );

    expect(screen.getByText("Mika")).toBeInTheDocument();
    expect(
      screen.getByText(/There is one Mika per workspace/i),
    ).toBeInTheDocument();
  });

  it("explains that hidden builder sessions keep their binding but stop running", () => {
    // The plan can be non-empty with zero NAMED agents: a builder carrier is
    // invisible in the agent list but its work still gets cancelled, so the
    // dialog has to open and say so rather than let a silent teardown through.
    renderDialog(makePlan({ activeAgents: [], retainedAgentCount: 1 }));
    expect(
      screen.getByText(/hidden Agent Builder session stays attached/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Make this runtime private?"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/only run my own agents from now on/i),
    ).toBeInTheDocument();
  });

  it("re-renders the fresh plan and clears the confirmation when the set changed", async () => {
    mockRevoke.mockRejectedValueOnce(
      new MockApiError(409, {
        code: "runtime_visibility_plan_changed",
        active_agents: [
          { id: "agent-1", name: "Teammate Agent" },
          { id: "agent-2", name: "Second Agent" },
        ],
        archived_agent_count: 0,
        retained_agent_count: 0,
        mika_affected: false,
      }),
    );

    const { onRevoked } = renderDialog(makePlan());
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Make private" }));

    await waitFor(() =>
      expect(
        screen.getByText(/The affected agent set changed/i),
      ).toBeInTheDocument(),
    );
    // The newly bound agent is now part of the visible plan...
    expect(screen.getByText("Second Agent")).toBeInTheDocument();
    // ...the checkbox is unticked so the user has to approve the new plan...
    expect(
      (screen.getByRole("button", { name: "Make private" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    // ...and nothing was reported as done, because the server wrote nothing.
    expect(onRevoked).not.toHaveBeenCalled();
    expect(mockToastError).not.toHaveBeenCalled();

    // Re-confirming submits the NEW set, not the stale one.
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Make private" }));
    await waitFor(() =>
      expect(mockRevoke).toHaveBeenLastCalledWith({
        runtimeId: "rt-1",
        expectedActiveAgentIds: ["agent-1", "agent-2"],
      }),
    );
    await waitFor(() => expect(onRevoked).toHaveBeenCalled());
  });

  it("surfaces an unrelated failure as an error toast", async () => {
    mockRevoke.mockRejectedValueOnce(new Error("boom"));
    const { onRevoked } = renderDialog(makePlan());

    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByRole("button", { name: "Make private" }));

    await waitFor(() => expect(mockToastError).toHaveBeenCalledWith("boom"));
    expect(onRevoked).not.toHaveBeenCalled();
  });
});

describe("parseRuntimeRevokeConflict", () => {
  it("reads the impact plan off the PATCH refusal", () => {
    const conflict = parseRuntimeRevokeConflict(
      new MockApiError(409, {
        code: "runtime_visibility_has_foreign_agents",
        active_agents: [{ id: "agent-1", name: "Teammate Agent" }],
        archived_agent_count: 2,
        retained_agent_count: 1,
        mika_affected: true,
      }),
    );
    expect(conflict).toEqual({
      code: "runtime_visibility_has_foreign_agents",
      plan: {
        activeAgents: [{ id: "agent-1", name: "Teammate Agent" }],
        archivedAgentCount: 2,
        retainedAgentCount: 1,
        mikaAffected: true,
      },
    });
  });

  it("ignores anything that is not one of the two visibility conflicts", () => {
    // A dialog must never open on a body it did not understand: the user would
    // be confirming a plan nobody sent.
    expect(
      parseRuntimeRevokeConflict(
        new MockApiError(409, { code: "runtime_has_active_agents" }),
      ),
    ).toBeNull();
    expect(
      parseRuntimeRevokeConflict(
        new MockApiError(500, {
          code: "runtime_visibility_has_foreign_agents",
        }),
      ),
    ).toBeNull();
    expect(parseRuntimeRevokeConflict(new Error("network"))).toBeNull();
  });

  it("tolerates a malformed payload instead of throwing", () => {
    const conflict = parseRuntimeRevokeConflict(
      new MockApiError(409, {
        code: "runtime_visibility_plan_changed",
        active_agents: "not-an-array",
        archived_agent_count: "many",
      }),
    );
    expect(conflict?.plan.activeAgents).toEqual([]);
    expect(conflict?.plan.archivedAgentCount).toBe(0);
    expect(conflict?.plan.mikaAffected).toBe(false);
  });
});
