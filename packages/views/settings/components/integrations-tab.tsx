"use client";

import { LarkTab } from "./lark-tab";
import { SlackTab } from "./slack-tab";
import { DingTalkTab } from "./dingtalk-tab";
import { WecomTab } from "./wecom-tab";
import { useT } from "../../i18n";
import { SettingsSection, SettingsTab } from "./settings-layout";
import { IntegrationChannelIcon } from "./integration-channel-icon";

// Integrations is the umbrella tab for the platforms a team talks on: Lark,
// Slack, DingTalk and WeCom today, Linear etc. to follow. Each owns its own
// description and install flow; this tab is just the host, so a new messaging
// platform slots in without changing the IA.
//
// Sorted by what a connection gives you, not by who provides it (MUL-6232):
// code hosting moved to Repositories, and agent tooling — Composio — to MCP.
export function IntegrationsTab() {
  const { t } = useT("settings");

  return (
    <SettingsTab
      title={t(($) => $.page.tabs.integrations)}
      description={t(($) => $.integrations.page_description)}
    >
      <SettingsSection
        title={
          <span className="flex items-center gap-2">
            <IntegrationChannelIcon channel="lark" />
            {t(($) => $.lark.section_title)}
          </span>
        }
        description={t(($) => $.lark.page_description)}
      >
        <LarkTab />
      </SettingsSection>
      <SettingsSection
        title={
          <span className="flex items-center gap-2">
            <IntegrationChannelIcon channel="slack" />
            {t(($) => $.slack.section_title)}
          </span>
        }
        description={t(($) => $.slack.page_description)}
      >
        <SlackTab />
      </SettingsSection>
      <SettingsSection
        title={
          <span className="flex items-center gap-2">
            <IntegrationChannelIcon channel="dingtalk" />
            {t(($) => $.dingtalk.section_title)}
          </span>
        }
        description={t(($) => $.dingtalk.page_description)}
      >
        <DingTalkTab />
      </SettingsSection>
      <SettingsSection
        title={
          <span className="flex items-center gap-2">
            <IntegrationChannelIcon channel="wecom" />
            {t(($) => $.wecom.section_title)}
          </span>
        }
        description={t(($) => $.wecom.page_description)}
      >
        <WecomTab />
      </SettingsSection>
    </SettingsTab>
  );
}
