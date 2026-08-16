// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { ApiError } from "@multica/core/api";
import { configStore } from "@multica/core/config";
import { COMPOSIO_MCP_APPS_FLAG } from "@multica/core/feature-flags";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

// Composio moved from Integrations to MCP (MUL-6232). The flag and the 503
// probe that decide whether it renders moved with it, into ComposioSection, so
// the host does not carry them — these tests follow the gating, not the host.
const errorRef = vi.hoisted(() => ({ current: null as Error | null }));
const queryCallsRef = vi.hoisted(() => ({
  current: [] as { queryKey: unknown[]; enabled?: boolean }[],
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (opts: { queryKey: unknown[]; enabled?: boolean }) => {
    queryCallsRef.current.push(opts);
    return {
      data: undefined,
      error: opts.enabled === false ? null : errorRef.current,
      isError: opts.enabled !== false && errorRef.current != null,
    };
  },
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/composio", () => ({
  composioKeys: {
    all: ["composio"],
    toolkits: () => ["composio", "toolkits"],
    connections: () => ["composio", "connections"],
  },
  composioToolkitsOptions: () => ({ queryKey: ["composio", "toolkits"] }),
  composioConnectionsOptions: () => ({ queryKey: ["composio", "connections"] }),
}));

vi.mock("../../navigation", () => ({
  useNavigation: () => ({
    searchParams: new URLSearchParams("tab=mcp"),
    pathname: "/acme/settings",
    replace: vi.fn(),
  }),
}));

import { ComposioSection } from "./composio-tab";

afterEach(cleanup);

function renderSection() {
  return render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, settings: enSettings } }}>
      <ComposioSection />
    </I18nProvider>,
  );
}

beforeEach(() => {
  queryCallsRef.current = [];
  errorRef.current = null;
  configStore.getState().setFeatureFlags({ [COMPOSIO_MCP_APPS_FLAG]: true });
});

describe("ComposioSection", () => {
  it("renders nothing and disables the toolkits query when the flag is off", () => {
    configStore.getState().setFeatureFlags({ [COMPOSIO_MCP_APPS_FLAG]: false });

    const { container } = renderSection();

    expect(container).toBeEmptyDOMElement();
    expect(queryCallsRef.current[0]?.enabled).toBe(false);
  });

  it("renders with its own heading when the flag is on and the server has it", () => {
    renderSection();

    expect(screen.getByText("Composio")).toBeInTheDocument();
    expect(queryCallsRef.current[0]?.enabled).toBe(true);
  });

  it("renders nothing when the deployment reports 503", () => {
    errorRef.current = new ApiError("unavailable", 503, "Service Unavailable");

    const { container } = renderSection();

    expect(container).toBeEmptyDOMElement();
  });
});
