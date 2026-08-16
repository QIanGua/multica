// @vitest-environment jsdom

import { afterEach, describe, expect, it, vi } from "vitest";
import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enSettings from "../../locales/en/settings.json";

vi.mock("./lark-tab", () => ({
  LarkTab: () => <div data-testid="lark-tab" />,
}));

vi.mock("./slack-tab", () => ({
  SlackTab: () => <div data-testid="slack-tab" />,
}));

vi.mock("./dingtalk-tab", () => ({
  DingTalkTab: () => <div data-testid="dingtalk-tab" />,
}));

vi.mock("./wecom-tab", () => ({
  WecomTab: () => <div data-testid="wecom-tab" />,
}));

import { IntegrationsTab } from "./integrations-tab";

afterEach(cleanup);

function renderTab() {
  return render(
    <I18nProvider locale="en" resources={{ en: { common: enCommon, settings: enSettings } }}>
      <IntegrationsTab />
    </I18nProvider>,
  );
}

describe("Settings IntegrationsTab", () => {
  // Sorted by what a connection gives you rather than who provides it
  // (MUL-6232): code hosting is on the Repositories tab and Composio — hosted
  // MCP apps — is under MCP. What is left is the platforms a team talks on.
  it("hosts the messaging platforms and nothing else", () => {
    renderTab();

    for (const channel of ["lark", "slack", "dingtalk", "wecom"]) {
      expect(screen.getByTestId(`${channel}-tab`)).toBeInTheDocument();
    }
    expect(screen.queryByTestId("composio-tab")).toBeNull();
    expect(screen.queryByTestId("vcs-tab")).toBeNull();
  });

  it("shows each channel description below its icon and title", () => {
    renderTab();

    for (const channel of ["lark", "slack", "dingtalk", "wecom"]) {
      const icon = screen.getByTestId(`integration-channel-icon-${channel}`);
      const title = icon.closest("h3");
      const description = title?.nextElementSibling;
      expect(title).not.toBeNull();
      expect(description?.tagName).toBe("P");
      expect(description).toHaveClass("text-caption", "text-muted-foreground");
      expect(icon).not.toHaveClass("border");
      expect(icon).not.toHaveClass("bg-muted/40");
    }
  });

  // Reaching for a generic lucide glyph is how Slack and WeCom ended up sharing
  // one speech bubble, with nothing on the row saying which platform it was
  // (#6585). Requiring four distinct shapes is the cheap guard against a
  // regression to that.
  it("gives every channel its own brand mark", () => {
    renderTab();

    const shapes = ["lark", "slack", "dingtalk", "wecom"].map(
      (channel) => screen.getByTestId(`integration-channel-icon-${channel}`).innerHTML,
    );

    expect(new Set(shapes).size).toBe(shapes.length);
  });

});
