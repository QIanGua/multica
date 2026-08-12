import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

const listPlugins = vi.hoisted(() => vi.fn());
const installPlugin = vi.hoisted(() => vi.fn());
const enablePlugin = vi.hoisted(() => vi.fn());
const disablePlugin = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    listWorkspacePlugins: listPlugins,
    installWorkspacePlugin: installPlugin,
    enableWorkspacePlugin: enablePlugin,
    disableWorkspacePlugin: disablePlugin,
  },
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/auth", () => ({
  useAuthStore: (selector: (state: { user: { id: string } }) => unknown) =>
    selector({ user: { id: "user-1" } }),
}));

vi.mock("@multica/core/workspace/queries", () => ({
  memberListOptions: () => ({
    queryKey: ["workspaces", "workspace-1", "members"],
    queryFn: async () => [{ user_id: "user-1", role: "owner" }],
  }),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

import { PluginsTab } from "./plugins-tab";

const catalog = {
  plugin_key: "ai.multica.software-delivery",
  version: "1.0.0",
  bundled: true,
  contributions: ["review-readiness"],
};

const installed = {
  id: "installation-1",
  plugin_key: catalog.plugin_key,
  display_name: "Software Delivery",
  desired_version: "1.0.0",
  active_version: "1.0.0",
  enabled: false,
  desired_generation: 1,
  active_generation: 1,
  lifecycle_status: "installed",
  contributions: ["review-readiness"],
};

function Wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <I18nProvider
      locale="en"
      resources={{ en: { common: enCommon, settings: enSettings } }}
    >
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </I18nProvider>
  );
}

describe("PluginsTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    listPlugins
      .mockResolvedValueOnce({ plugins: [], catalog: [catalog] })
      .mockResolvedValueOnce({ plugins: [installed], catalog: [catalog] })
      .mockResolvedValueOnce({
        plugins: [{ ...installed, enabled: true, lifecycle_status: "active" }],
        catalog: [catalog],
      });
    installPlugin.mockResolvedValue(installed);
    enablePlugin.mockResolvedValue({
      ...installed,
      enabled: true,
      lifecycle_status: "active",
    });
  });

  it("keeps install and enable as separate visible actions", async () => {
    const user = userEvent.setup();
    render(<PluginsTab />, { wrapper: Wrapper });

    await user.click(await screen.findByRole("button", { name: "Install" }));
    await waitFor(() => expect(installPlugin).toHaveBeenCalledWith(
      "workspace-1",
      catalog.plugin_key,
    ));

    await user.click(await screen.findByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enablePlugin).toHaveBeenCalledWith(
      "workspace-1",
      "installation-1",
    ));

    expect(await screen.findByRole("button", { name: "Disable" })).toBeEnabled();
    expect(screen.getByText("review-readiness")).toBeInTheDocument();
    expect(screen.getByText("Active")).toBeInTheDocument();
  });
});
