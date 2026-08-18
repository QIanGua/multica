// @vitest-environment jsdom

import React from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithI18n } from "../test/i18n";

// Runtime list drives the worktree version gate. Each test sets the reported
// CLI version before rendering.
let runtimeCliVersion = "9.9.9";
// What the fake runtime row says about worktree support. This — not the version
// string — is what the gate reads now (MUL-5707), and the three values are
// genuinely different states: "yes", "a daemon that cannot", and a row written
// by a server too old to record capabilities at all (#7113).
let runtimeWorktreeMetadata: "advertised" | "daemon_cannot" | "server_recorded_nothing" =
  "advertised";
// What the desktop validator reports for the picked folder.
let pickedIsGitRepo: boolean | undefined = true;

const createProjectMock = vi.fn().mockResolvedValue({ id: "p1", slug: "p1" });

// jsdom has no IntersectionObserver; the modal's scroll affordances construct
// one, and the resulting rejection aborts the submit handler mid-flight.
class NoopIntersectionObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() {
    return [];
  }
}
vi.stubGlobal("IntersectionObserver", NoopIntersectionObserver);

vi.mock("@tanstack/react-query", () => ({
  // Query-aware: the modal issues several. Only the runtime list matters here;
  // returning runtimes for all of them would feed them to the member/agent
  // pickers, which expect a different shape.
  useQuery: (options: { queryKey?: unknown[] }) => {
    const key = options?.queryKey?.[0];
    if (key === "members" || key === "agents") return { data: [] };
    return {
      data: [
        {
          daemon_id: "daemon-1",
          metadata: {
            cli_version: runtimeCliVersion,
            // A capability-aware server always writes the key — null when the
            // daemon sent no header — so its absence means the SERVER is old.
            ...(runtimeWorktreeMetadata === "advertised"
              ? { capabilities: ["local-worktree-v1"] }
              : runtimeWorktreeMetadata === "daemon_cannot"
                ? { capabilities: ["skill-bundles-v1"] }
                : {}),
          },
        },
      ],
    };
  },
  // runtimeListOptions builds its descriptor with this; the mocked useQuery
  // reads queryKey off it, so an identity passthrough is enough.
  queryOptions: (options: unknown) => options,
}));

vi.mock("@multica/core/projects/mutations", () => ({
  useCreateProject: () => ({ mutateAsync: createProjectMock }),
}));

vi.mock("@multica/core/projects", () => ({
  useProjectDraftStore: (selector: (state: unknown) => unknown) =>
    selector({
      draft: {
        title: "Game Client",
        description: "",
        status: "planned",
        priority: "none",
        icon: "📁",
        leadId: null,
        leadType: null,
        startDate: null,
        dueDate: null,
        repos: [],
      },
      setDraft: vi.fn(),
      clearDraft: vi.fn(),
      resetDraft: vi.fn(),
    }),
}));

// The worktree gate asks the live backend, not the stored runtime row.
let serverVersion = "";
vi.mock("@multica/core/config", () => ({
  useConfigStore: (selector: (state: { serverVersion: string }) => unknown) =>
    selector({ serverVersion }),
}));

vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "workspace-1", slug: "ws", repos: [] }),
  useWorkspacePaths: () => ({ projectDetail: (id: string) => `/ws/projects/${id}` }),
}));
vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({ queryKey: ["members"], queryFn: vi.fn() }),
  agentListOptions: () => ({ queryKey: ["agents"], queryFn: vi.fn() }),
}));
vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({ getActorName: vi.fn() }),
}));
vi.mock("../navigation", () => ({ useNavigation: () => ({ push: vi.fn() }) }));
vi.mock("../editor", () => {
  const ContentEditor = React.forwardRef<HTMLTextAreaElement, { placeholder?: string }>(
    ({ placeholder }, ref) => <textarea ref={ref} placeholder={placeholder} />,
  );
  ContentEditor.displayName = "ContentEditor";
  return {
    ContentEditor,
    TitleEditor: ({
      placeholder,
      onChange,
    }: {
      placeholder?: string;
      onChange?: (value: string) => void;
    }) => <input placeholder={placeholder} onChange={(e) => onChange?.(e.target.value)} />,
  };
});
vi.mock("../issues/components/priority-icon", () => ({ PriorityIcon: () => <span /> }));
vi.mock("../common/actor-avatar", () => ({ ActorAvatar: () => <span /> }));
vi.mock("../projects/components/project-start-date-picker", () => ({
  ProjectStartDatePicker: () => <button type="button">Start date</button>,
}));
vi.mock("../projects/components/project-due-date-picker", () => ({
  ProjectDueDatePicker: () => <button type="button">Due date</button>,
}));

// Desktop-only surface: without these the Local directory tab never renders.
vi.mock("../platform/local-directory", () => ({
  isDesktopShell: () => true,
  pickDirectory: () =>
    Promise.resolve({ ok: true, path: "/Users/dev/work/game-client", basename: "game-client" }),
  validateLocalDirectory: () => Promise.resolve({ ok: true, is_git_repo: pickedIsGitRepo }),
}));
vi.mock("../platform/use-local-daemon-status", () => ({
  useLocalDaemonStatus: () => ({ daemonId: "daemon-1", deviceName: "MacBook", running: true }),
}));

// Render overlays inline so their contents are assertable.
vi.mock("@multica/ui/components/ui/dialog", () => ({
  Dialog: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  DialogContent: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  DialogTitle: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));
vi.mock("@multica/ui/components/ui/popover", () => ({
  Popover: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  PopoverTrigger: ({ render }: { render: React.ReactNode }) => <>{render}</>,
  PopoverContent: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
}));
vi.mock("@multica/ui/components/ui/dropdown-menu", () => ({
  DropdownMenu: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  DropdownMenuTrigger: ({ render }: { render: React.ReactNode }) => <>{render}</>,
  DropdownMenuContent: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  DropdownMenuItem: ({ children, onClick }: { children: React.ReactNode; onClick?: () => void }) => (
    <button type="button" onClick={onClick}>
      {children}
    </button>
  ),
}));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { CreateProjectModal, buildLocalDirectoryResourceRef } from "./create-project";

async function pickLocalDirectory(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole("button", { name: /Local directory/i }));
  await user.click(screen.getByRole("button", { name: /Choose directory/i }));
  await waitFor(() => expect(screen.getByText("/Users/dev/work/game-client")).toBeInTheDocument());
}

describe("CreateProjectModal — local directory execution mode", () => {
  beforeEach(() => {
    createProjectMock.mockClear();
    runtimeCliVersion = "9.9.9";
    runtimeWorktreeMetadata = "advertised";
    serverVersion = "";
    pickedIsGitRepo = true;
  });

  // Preselection, not a silent default change: a git repo the runtime can
  // actually run worktree mode on starts on parallel, because that is the mode
  // that fits — and the user sees it selected in a control they can flip before
  // creating anything.
  it("preselects parallel for a git repository", async () => {
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("button", { name: /^Parallel$/i })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Run in parallel, isolated/i })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("preselects direct for a plain folder", async () => {
    pickedIsGitRepo = false;
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("button", { name: /^Direct$/i })).toBeInTheDocument();
  });

  // A runtime that cannot run the mode must not be preselected into it, or the
  // create call would be rejected by the server's version gate.
  it("preselects direct when the daemon does not support it, even for a git repository", async () => {
    runtimeWorktreeMetadata = "daemon_cannot";
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("button", { name: /^Direct$/i })).toBeInTheDocument();
  });

  // Older desktop builds do not report is_git_repo. The option stays available
  // (the daemon has the final say), but we do not guess parallel on the user's
  // behalf without evidence.
  it("preselects direct when the desktop build cannot report git-ness", async () => {
    pickedIsGitRepo = undefined;
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("button", { name: /^Direct$/i })).toBeInTheDocument();
    // Still selectable — unknown is permissive about what the user MAY choose.
    expect(screen.getByRole("radio", { name: /Run in parallel, isolated/i })).not.toBeDisabled();
  });

  // The preselection is a starting point, not a correction: once the user says
  // what they want, a later folder change must not silently undo it.
  it("keeps an explicit choice when the folder changes to another git repository", async () => {
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);
    await user.click(screen.getByRole("radio", { name: /Edit this folder directly/i }));
    expect(screen.getByRole("button", { name: /^Direct$/i })).toBeInTheDocument();

    // Re-pick (the mocked picker returns the same git repo).
    await user.click(screen.getByRole("button", { name: /Change directory/i }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /^Direct$/i })).toBeInTheDocument(),
    );
  });

  // The whole point of this entry point: the mode is part of setting the
  // folder up, not something to go and change afterwards.
  it("offers the mode next to the change-directory action once a folder is picked", async () => {
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    // The compact button uses the short label; the picker carries the full
    // title. A git repo preselects parallel, so that is the label here.
    expect(screen.getByRole("button", { name: /^Parallel$/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Change directory/i })).toBeInTheDocument();
  });

  // The server refuses to save a worktree resource the machine cannot run, and
  // here that rejection would fail the whole project creation — so the option
  // has to be blocked before submit, not after.
  it("blocks parallel mode when the daemon does not support it", async () => {
    runtimeWorktreeMetadata = "daemon_cannot";
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    const worktreeOption = screen.getByRole("radio", { name: /Run in parallel, isolated/i });
    expect(worktreeOption).toBeDisabled();
    expect(screen.getByText(/Update Multica there/i)).toBeInTheDocument();
    // Nothing gates on a version any more; quoting the old floor is what told a
    // user on v0.4.28 to upgrade past v0.4.24 (#7113).
    expect(screen.queryByText(/0\.4\.24/)).not.toBeInTheDocument();
  });

  // Self-hosted skew: Desktop ships its own renderer and daemon and updates
  // both, so it routinely runs ahead of a hand-upgraded backend. The daemon is
  // fine here — the backend never recorded what it advertised, and says so
  // itself via /api/config.
  it("blames the backend when the running server predates the handshake", async () => {
    runtimeWorktreeMetadata = "server_recorded_nothing";
    runtimeCliVersion = "v0.4.28";
    serverVersion = "0.4.24";
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("radio", { name: /Run in parallel, isolated/i })).toBeDisabled();
    expect(screen.getByText(/Multica server is older/i)).toBeInTheDocument();
    // Telling this user to update the machine is the bug: it is already newest.
    expect(screen.queryByText(/Update Multica there/i)).not.toBeInTheDocument();
  });

  // A pre-v0.4.2 self-hosted backend names no version and records no
  // capabilities: the machine is the only runtime here, so nothing else
  // identifies the server either. Sending this operator to restart the machine
  // would loop against the same blind backend forever.
  it("names both remedies when nothing identifies the backend", async () => {
    runtimeWorktreeMetadata = "server_recorded_nothing";
    runtimeCliVersion = "v0.4.28";
    serverVersion = "";
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("radio", { name: /Run in parallel, isolated/i })).toBeDisabled();
    expect(screen.getByText(/can't confirm whether that machine/i)).toBeInTheDocument();
  });

  // Same stored row, backend since upgraded. Upgrading it again fixes nothing —
  // the row simply predates the upgrade, so the machine has to register again.
  it("asks for a re-register when the backend is current but the row is old", async () => {
    runtimeWorktreeMetadata = "server_recorded_nothing";
    runtimeCliVersion = "v0.4.28";
    serverVersion = "0.4.28";
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("radio", { name: /Run in parallel, isolated/i })).toBeDisabled();
    expect(screen.getByText(/Restart Multica there/i)).toBeInTheDocument();
    expect(screen.queryByText(/Multica server is older/i)).not.toBeInTheDocument();
  });

  it("blocks parallel mode for a folder that is not a git repository", async () => {
    pickedIsGitRepo = false;
    const user = userEvent.setup();
    renderWithI18n(<CreateProjectModal onClose={vi.fn()} />);

    await pickLocalDirectory(user);

    expect(screen.getByRole("radio", { name: /Run in parallel, isolated/i })).toBeDisabled();
    expect(screen.getByText(/not a git repository/i)).toBeInTheDocument();
  });
});

// The payload is what the server stores and the daemon later reads; a missing
// or wrong execution_mode is invisible until a task actually runs. Asserted on
// the builder directly rather than through a full modal submit, which drags in
// unrelated modal machinery without testing anything more about this contract.
describe("buildLocalDirectoryResourceRef", () => {
  it("carries the chosen mode", () => {
    expect(
      buildLocalDirectoryResourceRef({
        localPath: "/Users/dev/work/game-client",
        daemonId: "daemon-1",
        label: "game-client",
        mode: "worktree",
      }),
    ).toEqual({
      local_path: "/Users/dev/work/game-client",
      daemon_id: "daemon-1",
      label: "game-client",
      execution_mode: "worktree",
    });
  });

  it("defaults are explicit: in_place is sent, not omitted", () => {
    // The server treats an absent field as in_place, but sending it keeps the
    // stored ref self-describing for anyone reading it later.
    expect(
      buildLocalDirectoryResourceRef({
        localPath: "/tmp/x",
        daemonId: "d",
        label: null,
        mode: "in_place",
      }),
    ).toEqual({ local_path: "/tmp/x", daemon_id: "d", execution_mode: "in_place" });
  });
});
