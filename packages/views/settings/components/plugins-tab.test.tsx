import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const mockInstall = vi.hoisted(() => vi.fn());
const mockSetEnabled = vi.hoisted(() => vi.fn());
const mockUpgrade = vi.hoisted(() => vi.fn());
const mockRollback = vi.hoisted(() => vi.fn());
const mockUninstall = vi.hoisted(() => vi.fn());
const mockConfigureRemoteMCP = vi.hoisted(() => vi.fn());
const mockTestRemoteMCP = vi.hoisted(() => vi.fn());
const mockApproveRemoteMCP = vi.hoisted(() => vi.fn());
const mockRevokeRemoteMCP = vi.hoisted(() => vi.fn());
const mockStartRemoteMCPOAuth = vi.hoisted(() => vi.fn());
const mockOpenExternal = vi.hoisted(() => vi.fn());
const mockRefetch = vi.hoisted(() => vi.fn());

const data = vi.hoisted(() => ({
  catalog: {
    supported: true,
    diagnostics: [] as unknown[],
    releases: [{
      plugin_key: "ai.multica.software-delivery",
      name: "Software Delivery",
      description: "Official Multica software-delivery workflow skills.",
      version: "1.1.0",
      publisher: "multica",
      publisher_type: "official",
      trust_tier: "official",
      source_kind: "bundled",
      source_ref: "bundled://ai.multica.software-delivery/1.1.0",
      requested_capabilities: ["agent.skill.contribute"],
      host_api: ">=1.0.0 <2.0.0",
      required_daemon_features: ["execution-manifest-v1", "agent-skill-v1"],
      signature_key_id: "multica-plugin-release-2026-01",
      signature_verified: true,
      manifest_digest: "sha256:manifest",
      archive_digest: "sha256:archive",
      artifact_digest: "sha256:artifact",
      compatible: true,
      contributions: [{
        key: "review-readiness",
        type: "agent.skill.v1",
        name: "review-readiness",
        description: "Verify that a software change is ready for review and handoff.",
        entry_path: "skills/review-readiness/SKILL.md",
        entry_digest: "sha256:entry",
      }],
    }],
  },
  installed: { plugins: [] as Array<Record<string, unknown>> },
  agents: [{ id: "agent-1", name: "Reviewer", archived_at: null }],
  members: [{ user_id: "member-1", name: "Alice", email: "alice@example.com", role: "admin" }],
  role: "owner" as "owner" | "admin" | "member",
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey?: readonly string[] }) => {
    const key = options.queryKey?.join(":") ?? "";
    if (key.includes("catalog")) return { data: data.catalog, isPending: false, isError: false, refetch: mockRefetch };
    if (key.includes("installed")) return { data: data.installed, isPending: false, isError: false, refetch: mockRefetch };
    if (key.includes("members")) return { data: data.members, isPending: false, isError: false, refetch: mockRefetch };
    return { data: data.agents, isPending: false, isError: false, refetch: mockRefetch };
  },
}));

vi.mock("@multica/core/plugins", () => ({
  comparePluginVersions: (left: string, right: string) => left.localeCompare(right),
  pluginCatalogOptions: () => ({ queryKey: ["plugins", "catalog"] }),
  pluginInstallationsOptions: () => ({ queryKey: ["plugins", "installed"] }),
  useInstallPlugin: () => ({ mutateAsync: mockInstall, isPending: false }),
  useSetPluginEnabled: () => ({ mutateAsync: mockSetEnabled, isPending: false }),
  useUpgradePlugin: () => ({ mutateAsync: mockUpgrade, isPending: false }),
  useRollbackPlugin: () => ({ mutateAsync: mockRollback, isPending: false }),
  useUninstallPlugin: () => ({ mutateAsync: mockUninstall, isPending: false }),
  useConfigurePluginRemoteMCP: () => ({ mutateAsync: mockConfigureRemoteMCP, isPending: false }),
  useTestPluginRemoteMCP: () => ({ mutateAsync: mockTestRemoteMCP, isPending: false }),
  useApprovePluginRemoteMCPTools: () => ({ mutateAsync: mockApproveRemoteMCP, isPending: false }),
  useRevokePluginRemoteMCPCredential: () => ({ mutateAsync: mockRevokeRemoteMCP, isPending: false }),
  useStartPluginRemoteMCPOAuth: () => ({ mutateAsync: mockStartRemoteMCPOAuth, isPending: false }),
}));

vi.mock("../../platform/open-external", () => ({ openExternal: mockOpenExternal }));
vi.mock("../../platform/local-directory", () => ({ isDesktopShell: () => true }));

vi.mock("@multica/core/workspace/queries", () => ({
  agentListOptions: () => ({ queryKey: ["agents"] }),
  memberListOptions: () => ({ queryKey: ["members"] }),
}));

vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "workspace-1", name: "Acme", slug: "acme" }),
}));

vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: data.role, isLoading: false }),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

// Base UI's Dialog is a portal that's awkward under jsdom — strip it to
// pass-through wrappers. The install/review flows under test live in the
// dialog bodies, not in Base UI itself.
vi.mock("@multica/ui/components/ui/dialog", () => ({
  Dialog: ({ children, open }: { children: ReactNode; open?: boolean }) =>
    open ? <div role="dialog">{children}</div> : null,
  DialogContent: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogHeader: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogTitle: ({ children }: { children: ReactNode }) => <h1>{children}</h1>,
  DialogDescription: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogFooter: ({ children }: { children: ReactNode }) => <div>{children}</div>,
}));

import { PluginsTab } from "./plugins-tab";

const TEST_RESOURCES = { en: { common: enCommon, settings: enSettings } };

function Wrapper({ children }: { children: ReactNode }) {
  return <I18nProvider locale="en" resources={TEST_RESOURCES}>{children}</I18nProvider>;
}

function skillOnlyInstallation(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "installation-1",
    plugin_key: "ai.multica.software-delivery",
    display_name: "Software Delivery",
    description: "Official Multica software-delivery workflow skills.",
    desired_version: "1.1.0",
    active_version: "1.1.0",
    enabled: false,
    desired_generation: 1,
    active_generation: 1,
    lifecycle_status: "installed",
    publisher: "multica",
    publisher_type: "official",
    trust_tier: "official",
    source_kind: "bundled",
    source_ref: "bundled://ai.multica.software-delivery/1.1.0",
    manifest_digest: "sha256:manifest",
    archive_digest: "sha256:archive",
    artifact_digest: "sha256:artifact",
    signature_verified: true,
    requested_capabilities: ["agent.skill.contribute"],
    available_versions: ["1.1.0"],
    contributions: ["review-readiness"],
    contribution_details: [],
    bindings: [],
    remote_mcp: [],
    ...overrides,
  };
}

function remoteMCPInstallation(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "installation-2",
    plugin_key: "dev.acme.search",
    display_name: "Search",
    desired_version: "0.1.0",
    active_version: "0.1.0",
    enabled: true,
    desired_generation: 1,
    active_generation: 1,
    lifecycle_status: "installed",
    health_state: "degraded",
    health_reason: "needs_setup",
    publisher: "acme.internal",
    publisher_type: "private_dev",
    trust_tier: "private_dev",
    source_kind: "private_dev",
    source_ref: "private://sha256:search",
    uploader_id: "member-1",
    manifest_digest: "sha256:manifest",
    archive_digest: "sha256:archive",
    artifact_digest: "sha256:artifact",
    signature_verified: false,
    requested_capabilities: ["tool.remote-mcp.connect"],
    available_versions: ["0.1.0"],
    contributions: ["search"],
    contribution_details: [],
    bindings: [{ scope_type: "workspace", scope_id: "workspace-1", enabled: true, revision: 1 }],
    remote_mcp: [{
      contribution_key: "search",
      default_endpoint: "https://default.example.test/mcp",
      preferred_auth: "oauth",
      supported_auth: ["oauth"],
      config_revision: 2,
      endpoint: "https://mcp.example.test/mcp",
      endpoint_domain: "mcp.example.test",
      auth_type: "oauth",
      public_config: {},
      failure_policy: "required",
      credential_state: "configured",
      approved_tools: [],
      discovered_tools: [
        { name: "search_docs", description: "Search the docs.", input_schema: {}, schema_digest: "sha256:a", risk: "read" },
        { name: "update_page", description: "Update a page.", input_schema: {}, schema_digest: "sha256:b", risk: "write" },
      ],
      discovered_schema_digest: "sha256:discovered",
      reviewed: false,
      ready: false,
    }],
    ...overrides,
  };
}

describe("PluginsTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    window.history.replaceState({}, "", "/");
    data.role = "owner";
    data.catalog.releases[0]!.compatible = true;
    data.catalog.releases[0]!.signature_verified = true;
    data.installed.plugins = [];
    mockInstall.mockResolvedValue({ id: "installation-9" });
    mockSetEnabled.mockResolvedValue({});
    mockApproveRemoteMCP.mockResolvedValue(undefined);
    mockUninstall.mockResolvedValue({});
    mockStartRemoteMCPOAuth.mockResolvedValue({ authorization_url: "https://auth.example.test/authorize" });
  });

  it("shows the needs-setup state for an enabled installation whose remote MCP is not ready", () => {
    data.installed.plugins = [remoteMCPInstallation()];
    render(<PluginsTab />, { wrapper: Wrapper });

    expect(screen.getByText("Needs setup")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Finish setup" })).toBeInTheDocument();
    expect(screen.getByRole("switch")).toBeChecked();
  });

  it("turns a disabled installation on through the workspace binding", async () => {
    const user = userEvent.setup();
    data.installed.plugins = [skillOnlyInstallation()];
    render(<PluginsTab />, { wrapper: Wrapper });

    expect(screen.getByText("Off")).toBeInTheDocument();
    await user.click(screen.getByRole("switch"));
    await waitFor(() => expect(mockSetEnabled).toHaveBeenCalledWith({
      installationId: "installation-1",
      enabled: true,
      binding: { scope_type: "workspace", scope_id: "workspace-1" },
    }));
    // The switch click must not bubble into the row and open the detail view.
    expect(screen.getByRole("button", { name: "Browse Marketplace" })).toBeInTheDocument();
  });

  it("turning off disables every enabled binding sequentially", async () => {
    const user = userEvent.setup();
    data.installed.plugins = [skillOnlyInstallation({
      enabled: true,
      bindings: [
        { scope_type: "workspace", scope_id: "workspace-1", enabled: true, revision: 1 },
        { scope_type: "agent", scope_id: "agent-1", enabled: true, revision: 1 },
        { scope_type: "agent", scope_id: "agent-2", enabled: false, revision: 1 },
      ],
    })];
    render(<PluginsTab />, { wrapper: Wrapper });

    await user.click(screen.getByRole("switch"));
    await waitFor(() => expect(mockSetEnabled).toHaveBeenCalledTimes(2));
    expect(mockSetEnabled).toHaveBeenNthCalledWith(1, {
      installationId: "installation-1",
      enabled: false,
      binding: { scope_type: "workspace", scope_id: "workspace-1" },
    });
    expect(mockSetEnabled).toHaveBeenNthCalledWith(2, {
      installationId: "installation-1",
      enabled: false,
      binding: { scope_type: "agent", scope_id: "agent-1" },
    });
  });

  it("installs from the marketplace dialog and enables the workspace binding", async () => {
    const user = userEvent.setup();
    render(<PluginsTab />, { wrapper: Wrapper });

    await user.click(screen.getByRole("button", { name: "Browse Marketplace" }));
    expect(screen.getByText("Plugin Marketplace")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Install" }));
    expect(screen.getByText("Install Software Delivery")).toBeInTheDocument();
    expect(screen.getByRole("checkbox")).toBeChecked();

    const installButtons = screen.getAllByRole("button", { name: "Install" });
    await user.click(installButtons[installButtons.length - 1]!);

    await waitFor(() => expect(mockInstall).toHaveBeenCalledWith({
      plugin_key: "ai.multica.software-delivery",
      version: "1.1.0",
    }));
    await waitFor(() => expect(mockSetEnabled).toHaveBeenCalledWith({
      installationId: "installation-9",
      enabled: true,
      binding: { scope_type: "workspace", scope_id: "workspace-1" },
    }));
  });

  it("auto-opens the review dialog in detail and approves the checked tools", async () => {
    const user = userEvent.setup();
    data.installed.plugins = [remoteMCPInstallation()];
    render(<PluginsTab />, { wrapper: Wrapper });

    await user.click(screen.getByText("Search"));
    expect(await screen.findByText("Review tools from Search")).toBeInTheDocument();
    expect(screen.getByText("Read-only · 1")).toBeInTheDocument();
    expect(screen.getByText("Write · 1")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Approve 2 tools, finish setup" }));
    await waitFor(() => expect(mockApproveRemoteMCP).toHaveBeenCalledWith({
      installationId: "installation-2",
      contributionKey: "search",
      tools: ["search_docs", "update_page"],
    }));
  });

  it("shows no connection section for a skill-only Plugin detail", async () => {
    const user = userEvent.setup();
    data.installed.plugins = [skillOnlyInstallation({
      enabled: true,
      bindings: [{ scope_type: "workspace", scope_id: "workspace-1", enabled: true, revision: 1 }],
    })];
    render(<PluginsTab />, { wrapper: Wrapper });

    await user.click(screen.getByText("Software Delivery"));
    expect(await screen.findByText("Availability")).toBeInTheDocument();
    expect(screen.getByText("About")).toBeInTheDocument();
    expect(screen.queryByText("Connection")).not.toBeInTheDocument();
    expect(screen.getByText("All agents")).toBeInTheDocument();
  });

  it("keeps the list read-only for members", () => {
    data.role = "member";
    data.installed.plugins = [skillOnlyInstallation()];
    render(<PluginsTab />, { wrapper: Wrapper });

    expect(screen.getByText("Read-only access")).toBeInTheDocument();
    // Base UI's Switch renders a span, so disabled surfaces as aria-disabled.
    expect(screen.getByRole("switch")).toHaveAttribute("aria-disabled", "true");
  });

  it("consumes a successful OAuth callback and refreshes installation state", async () => {
    window.history.replaceState({}, "", "/settings?tab=plugins&remote_mcp_connected=1#plugins");

    render(<PluginsTab />, { wrapper: Wrapper });

    await waitFor(() => expect(mockRefetch).toHaveBeenCalled());
    expect(window.location.search).toBe("?tab=plugins");
    expect(window.location.hash).toBe("#plugins");
  });

  it("returns from OAuth into the detail view of the installation pending review", async () => {
    window.history.replaceState({}, "", "/settings?tab=plugins&remote_mcp_connected=1");
    data.installed.plugins = [remoteMCPInstallation()];
    mockRefetch.mockResolvedValue({ data: { plugins: data.installed.plugins } });

    render(<PluginsTab />, { wrapper: Wrapper });

    expect(await screen.findByText("Review tools from Search")).toBeInTheDocument();
    expect(window.location.search).toBe("?tab=plugins");
  });
});
