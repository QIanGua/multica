import { fireEvent, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { SidebarProvider, useSidebar } from "@multica/ui/components/ui/sidebar";
import { configStore } from "@multica/core/config";
import {
  BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG,
  PLUGINS_V1_FLAG,
} from "@multica/core/feature-flags";
import { renderWithI18n } from "../../test/i18n";

// This file tests the settings SHELL — the chrome around the tabs — so every
// tab panel is stubbed out. Their contents have their own test files.
const stub = vi.hoisted(
  () => (name: string) => () => ({ [name]: () => <div>{name}</div> }),
);
vi.mock("./account-tab", stub("AccountTab"));
vi.mock("./preferences-tab", stub("PreferencesTab"));
vi.mock("./chat-tab", stub("ChatTab"));
vi.mock("./issue-tab", stub("IssueTab"));
vi.mock("./tokens-tab", stub("TokensTab"));
vi.mock("./workspace-tab", stub("WorkspaceTab"));
vi.mock("./members-tab", stub("MembersTab"));
vi.mock("./repositories-tab", stub("RepositoriesTab"));
vi.mock("./integrations-tab", stub("IntegrationsTab"));
vi.mock("./notifications-tab", stub("NotificationsTab"));
vi.mock("./workspace-issue-tab", stub("WorkspaceIssueTab"));
vi.mock("./keyboard-shortcuts-tab", stub("KeyboardShortcutsTab"));
vi.mock("./plugins-tab", stub("PluginsTab"));
vi.mock("./mcp-tab", stub("McpTab"));
vi.mock("./billing-tab", stub("BillingTab"));
// Labs is gated on a real exported constant, so it is stubbed by hand rather
// than through `stub()`.
vi.mock("./labs-tab", () => ({
  LabsTab: () => <div>LabsTab</div>,
  LABS_HAS_EXPERIMENTS: false,
}));

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ name: "Acme" }),
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

// Compact by default: that is the width where the nav is a sheet and this
// trigger is the only way to reach it.
const layout = { compact: true };
vi.mock("@multica/ui/hooks/use-mobile", () => ({
  useIsMobile: () => layout.compact,
  useIsCompact: () => layout.compact,
}));

import { SettingsPage } from "./settings-page";

function NavStateProbe() {
  const { openMobile } = useSidebar();
  return <div data-testid="nav-open">{String(openMobile)}</div>;
}

function trigger() {
  return screen.getByRole("button", { name: "Toggle Sidebar" });
}

beforeEach(() => {
  layout.compact = true;
  navigationState.search = "";
  configStore.getState().setFeatureFlags({});
  replace.mockClear();
});

describe("SettingsPage nav trigger", () => {
  it("opens the nav from settings at compact widths", () => {
    // Settings builds its own chrome instead of a PageHeader, so without this
    // control a touch user who lands here has no way back to the nav at all —
    // the keyboard shortcut is not an answer on a tablet.
    renderWithI18n(
      <SidebarProvider>
        <NavStateProbe />
        <SettingsPage />
      </SidebarProvider>,
    );

    expect(screen.getByTestId("nav-open").textContent).toBe("false");

    fireEvent.click(trigger());

    expect(screen.getByTestId("nav-open").textContent).toBe("true");
  });

  it("hides the trigger only where the nav is a permanent column", () => {
    // The nav is in-flow from `xl` up, so the control is CSS-gated rather than
    // unmounted — jsdom applies no stylesheet, hence the class assertion.
    renderWithI18n(
      <SidebarProvider>
        <SettingsPage />
      </SidebarProvider>,
    );

    expect(trigger().className).toContain("xl:hidden");
  });

  it("still renders standalone, without a sidebar around it", () => {
    // Desktop mounts settings inside its own shell; the trigger has to no-op
    // rather than throw when there is no SidebarProvider above it.
    renderWithI18n(<SettingsPage />);

    expect(
      screen.queryByRole("button", { name: "Toggle Sidebar" }),
    ).not.toBeInTheDocument();
    expect(screen.getByText("Settings")).toBeInTheDocument();
  });
});

describe("SettingsPage Plugin feature flag", () => {
  it("hides Plugins and falls back from a direct tab URL when disabled", () => {
    navigationState.search = "tab=plugins";

    renderWithI18n(<SettingsPage />);

    expect(screen.queryByRole("tab", { name: "Plugins" })).not.toBeInTheDocument();
    expect(screen.queryByText("PluginsTab")).not.toBeInTheDocument();
    expect(screen.getByText("AccountTab")).toBeInTheDocument();
  });

  it("shows and mounts Plugins when explicitly enabled", () => {
    navigationState.search = "tab=plugins";
    configStore.getState().setFeatureFlags({ [PLUGINS_V1_FLAG]: true });

    renderWithI18n(<SettingsPage />);

    expect(screen.getByRole("tab", { name: "Plugins" })).toBeInTheDocument();
    expect(screen.getByText("PluginsTab")).toBeInTheDocument();
  });
});

describe("SettingsPage workspace subscription feature flag", () => {
  it("hides Billing and falls back to Workspace General from a direct URL", () => {
    navigationState.search = "tab=billing";

    renderWithI18n(<SettingsPage />);

    expect(
      screen.queryByRole("tab", { name: "Billing" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("BillingTab")).not.toBeInTheDocument();
    expect(screen.getByText("WorkspaceTab")).toBeInTheDocument();
  });

  it("shows and mounts Billing only when explicitly enabled", () => {
    navigationState.search = "tab=billing";
    configStore.getState().setFeatureFlags({
      [BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG]: true,
    });

    renderWithI18n(<SettingsPage />);

    expect(screen.getByRole("tab", { name: "Billing" })).toBeInTheDocument();
    expect(screen.getByText("BillingTab")).toBeInTheDocument();
  });
});

describe("SettingsPage information architecture", () => {
  it("groups the nav by scope: account, then workspace", () => {
    renderWithI18n(<SettingsPage />);

    expect(screen.getByText("Account")).toBeInTheDocument();
    expect(screen.getByText("Acme")).toBeInTheDocument();
    // No desktop tabs were injected, so that group must not announce itself.
    expect(screen.queryByText("Desktop (This Device)")).not.toBeInTheDocument();
  });

  it("gives the desktop app its own group instead of appending to Account", () => {
    renderWithI18n(
      <SettingsPage
        desktopTabs={[
          {
            value: "daemon",
            label: "Daemon",
            icon: () => <span />,
            content: <div>DaemonTab</div>,
          },
        ]}
      />,
    );

    expect(screen.getByText("Desktop (This Device)")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Daemon" })).toBeInTheDocument();
  });

  it("hides Labs while it has no experiments", () => {
    // An entry that leads to an empty page is worse than no entry (MUL-6232).
    navigationState.search = "tab=labs";

    renderWithI18n(<SettingsPage />);

    expect(screen.queryByRole("tab", { name: "Labs" })).not.toBeInTheDocument();
    expect(screen.queryByText("LabsTab")).not.toBeInTheDocument();
    expect(screen.getByText("AccountTab")).toBeInTheDocument();
  });
});

describe("SettingsPage collapsed tab redirects", () => {
  it.each([
    ["tab=github", "RepositoriesTab"],
    ["tab=composio", "McpTab"],
    ["tab=lark", "IntegrationsTab"],
    ["tab=labels", "WorkspaceIssueTab"],
    ["tab=properties", "WorkspaceIssueTab"],
    ["tab=quick-actions", "WorkspaceIssueTab"],
  ])("sends a %s bookmark to %s", (search, expected) => {
    navigationState.search = search;

    renderWithI18n(<SettingsPage />);

    expect(screen.getByText(expected)).toBeInTheDocument();
  });

  it("keeps the account Issue tab distinct from the workspace one", () => {
    // Both are called "Issue"; only the query value tells them apart.
    navigationState.search = "tab=issue";
    renderWithI18n(<SettingsPage />);
    expect(screen.getByText("IssueTab")).toBeInTheDocument();
    expect(screen.queryByText("WorkspaceIssueTab")).not.toBeInTheDocument();
  });

  it("mounts the workspace Issue tab under its own value", () => {
    navigationState.search = "tab=workspace-issue";
    renderWithI18n(<SettingsPage />);
    expect(screen.getByText("WorkspaceIssueTab")).toBeInTheDocument();
    expect(screen.queryByText("IssueTab")).not.toBeInTheDocument();
  });
});
