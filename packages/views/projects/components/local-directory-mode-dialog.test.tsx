// @vitest-environment jsdom

import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { LocalDirectoryExecutionMode } from "@multica/core/types";
import enProjects from "../../locales/en/projects.json";
import enCommon from "../../locales/en/common.json";
import { LocalDirectoryModeDialog } from "./local-directory-mode-dialog";
import type { WorktreeUnavailableReason } from "./local-directory-mode-dialog";

const TEST_RESOURCES = { en: { projects: enProjects, common: enCommon } };

function renderDialog(
  overrides: {
    value?: LocalDirectoryExecutionMode;
    unavailableReason?: WorktreeUnavailableReason;
    errorMessage?: string;
    onConfirm?: (mode: LocalDirectoryExecutionMode) => void;
  } = {},
) {
  const onConfirm = overrides.onConfirm ?? vi.fn();
  const { unmount } = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <LocalDirectoryModeDialog
        open
        onOpenChange={() => {}}
        path="/Users/dev/work/game-client"
        value={overrides.value ?? "in_place"}
        unavailableReason={overrides.unavailableReason}
        errorMessage={overrides.errorMessage}
        confirmLabel="Save"
        onConfirm={onConfirm}
      />
    </I18nProvider>,
  );
  return { onConfirm, unmount };
}

function worktreeOption(): HTMLElement {
  return screen.getAllByRole("radio")[1] as HTMLElement;
}

describe("LocalDirectoryModeDialog", () => {
  it("leads with what the user gets back, not the mode identifiers", () => {
    renderDialog();
    // The identifiers stay visible as a secondary hint for anyone cross-
    // referencing the CLI or docs, but the decision is framed by outcome.
    expect(screen.getByText("Edit this folder directly")).toBeTruthy();
    expect(screen.getByText("Run in parallel, isolated")).toBeTruthy();
    expect(screen.getByText("in_place")).toBeTruthy();
    expect(screen.getByText("worktree")).toBeTruthy();
    expect(screen.getByText("/Users/dev/work/game-client")).toBeTruthy();
  });

  it("marks the current mode as selected", () => {
    renderDialog({ value: "worktree" });
    expect(worktreeOption().getAttribute("aria-checked")).toBe("true");
    expect(screen.getAllByRole("radio")[0]?.getAttribute("aria-checked")).toBe(
      "false",
    );
  });

  it("confirms the newly picked mode, not the one it opened with", () => {
    const onConfirm = vi.fn();
    renderDialog({ value: "in_place", onConfirm });

    fireEvent.click(worktreeOption());
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(onConfirm).toHaveBeenCalledWith("worktree");
  });

  // A non-git folder cannot produce a branch, so offering the option would
  // guarantee the user's first task fails. Disable it where they choose.
  it("disables parallel mode for a non-git folder and says why", () => {
    const onConfirm = vi.fn();
    renderDialog({ unavailableReason: "not_git", onConfirm });

    const option = worktreeOption();
    expect(option.hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/not a git repository/i)).toBeTruthy();

    fireEvent.click(option);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    // Still the mode it opened with — the disabled option cannot be selected.
    expect(onConfirm).toHaveBeenCalledWith("in_place");
  });

  // The server refuses the save when the machine cannot run the mode; saying so
  // up front beats a bare 422 after the user has committed to the choice.
  it("disables parallel mode for a daemon that cannot run it", () => {
    renderDialog({ unavailableReason: "daemon_outdated" });

    expect(worktreeOption().hasAttribute("disabled")).toBe(true);
    expect(screen.getByText(/Update Multica there/i)).toBeTruthy();
  });

  // #7113: nothing has gated on a version since MUL-5707, so quoting the old
  // floor told a user on v0.4.28 they needed v0.4.24 or newer. Any version
  // number in this copy is a regression, whichever blocker fired.
  it("never quotes a daemon version at the user", () => {
    for (const reason of ["daemon_outdated", "server_outdated"] as const) {
      const { unmount } = renderDialog({ unavailableReason: reason });
      expect(screen.queryByText(/0\.4\.24/)).toBeNull();
      expect(screen.queryByText(/an older version/i)).toBeNull();
      unmount();
    }
  });

  // The remedy is on the backend, and no amount of updating the machine can
  // reach it — so this must not read as "your machine is out of date".
  it("blames the backend, not the machine, when the server records no capabilities", () => {
    renderDialog({ unavailableReason: "server_outdated" });

    expect(worktreeOption().hasAttribute("disabled")).toBe(true);
    const notice = screen.getByText(/Multica server is older/i);
    // The server floor is the one version worth naming: it is what the operator
    // upgrades past, and it is not the daemon's.
    expect(notice.textContent).toContain("0.4.25");
  });

  it("shows a server rejection inline so the dialog stays actionable", () => {
    renderDialog({ errorMessage: "daemon is too old to run worktree mode" });
    expect(screen.getByText(/too old to run worktree mode/i)).toBeTruthy();
  });
});
