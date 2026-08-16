import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { renderWithI18n } from "../../test/i18n";

// The three panels have their own test files; this one covers the page that
// hosts them — which section opens, and how a link reaches it.
vi.mock("./properties-tab", () => ({ PropertiesPanel: () => <div>PropertiesPanel</div> }));
vi.mock("./labels-tab", () => ({ LabelsPanel: () => <div>LabelsPanel</div> }));
vi.mock("./quick-actions-tab", () => ({
  QuickActionsPanel: () => <div>QuickActionsPanel</div>,
}));

const replace = vi.fn();
const navigationState = { search: "" };
vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    searchParams: new URLSearchParams(navigationState.search),
    pathname: "/acme/settings",
    replace,
  }),
}));

import { WorkspaceIssueTab } from "./workspace-issue-tab";

beforeEach(() => {
  navigationState.search = "tab=workspace-issue";
  replace.mockClear();
});

describe("WorkspaceIssueTab", () => {
  it("opens on Properties by default", () => {
    renderWithI18n(<WorkspaceIssueTab />);

    expect(screen.getByText("PropertiesPanel")).toBeInTheDocument();
    expect(screen.queryByText("LabelsPanel")).not.toBeInTheDocument();
  });

  it("points at the account's Issue tab so the shared name is not ambiguous", () => {
    renderWithI18n(<WorkspaceIssueTab />);

    expect(screen.getByText(/Account › Issue/)).toBeInTheDocument();
  });

  it.each([
    ["section=labels", "LabelsPanel"],
    ["section=quick-actions", "QuickActionsPanel"],
    ["section=properties", "PropertiesPanel"],
  ])("opens %s directly", (section, expected) => {
    navigationState.search = `tab=workspace-issue&${section}`;

    renderWithI18n(<WorkspaceIssueTab />);

    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it.each([
    ["tab=labels", "LabelsPanel"],
    ["tab=properties", "PropertiesPanel"],
    ["tab=quick-actions", "QuickActionsPanel"],
  ])("honours the pre-merge %s value", (search, expected) => {
    // SettingsPage redirects these to this tab; the original value still says
    // which panel the link meant, so an old bookmark lands where it aimed.
    navigationState.search = search;

    renderWithI18n(<WorkspaceIssueTab />);

    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it("writes both tab and section when switching, so a reload holds", () => {
    navigationState.search = "tab=labels";

    renderWithI18n(<WorkspaceIssueTab />);
    fireEvent.click(screen.getByRole("tab", { name: "Quick Actions" }));

    expect(replace).toHaveBeenCalledWith(
      "/acme/settings?tab=workspace-issue&section=quick-actions",
    );
  });

  it("describes the open section, not the page as a whole", () => {
    // Each panel used to be a page with its own description; "labels come in
    // two independent sets" is not something the switcher shows on its own.
    navigationState.search = "tab=workspace-issue&section=labels";

    renderWithI18n(<WorkspaceIssueTab />);

    expect(screen.getByText(/organize issues and skills/i)).toBeInTheDocument();
  });
});
