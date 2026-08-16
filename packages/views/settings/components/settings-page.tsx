"use client";

import React from "react";
import {
  SlidersHorizontal,
  Key,
  Settings,
  Users,
  FolderGit2,
  FlaskConical,
  Bell,
  Plug,
  MessageCircle,
  Keyboard,
  ListTodo,
  Blocks,
  CreditCard,
  Server,
} from "lucide-react";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@multica/ui/components/ui/tabs";
import { useIsMobile } from "@multica/ui/hooks/use-mobile";
import { cn } from "@multica/ui/lib/utils";
import { useCurrentWorkspace } from "@multica/core/paths";
import { useFeatureEnabled } from "@multica/core/config";
import {
  BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG,
  PLUGINS_V1_FLAG,
} from "@multica/core/feature-flags";
import { useNavigation } from "../../navigation";
import { AccountTab } from "./account-tab";
import { PreferencesTab } from "./preferences-tab";
import { ChatTab } from "./chat-tab";
import { IssueTab } from "./issue-tab";
import { TokensTab } from "./tokens-tab";
import { WorkspaceTab } from "./workspace-tab";
import { MembersTab } from "./members-tab";
import { RepositoriesTab } from "./repositories-tab";
import { IntegrationsTab } from "./integrations-tab";
import { LabsTab, LABS_HAS_EXPERIMENTS } from "./labs-tab";
import { NotificationsTab } from "./notifications-tab";
import { WorkspaceIssueTab } from "./workspace-issue-tab";
import { KeyboardShortcutsTab } from "./keyboard-shortcuts-tab";
import { PluginsTab } from "./plugins-tab";
import { McpTab } from "./mcp-tab";
import { BillingTab } from "./billing-tab";
import { CollapsedNavTrigger } from "../../layout/page-header";
import { useT } from "../../i18n";

// Three scopes, in order of how far each setting reaches: the account travels
// with the person, the desktop group with this one machine, the workspace with
// the team. Within a scope the order answers, in turn: what is this → who can
// use it → how we work in it → what it is connected to. A new setting joins the
// end of ITS segment, not the end of the column — that is what kept the old
// order drifting into "newest feature last" (MUL-6232).
const ACCOUNT_TAB_KEYS = ["profile", "preferences", "shortcuts", "issue", "chat", "notifications", "tokens"] as const;
const ACCOUNT_TAB_ICONS = {
  // `profile` is the account's General page, so it carries the same gear as
  // the workspace's General; the group heading above it supplies the scope.
  profile: Settings,
  preferences: SlidersHorizontal,
  shortcuts: Keyboard,
  issue: ListTodo,
  chat: MessageCircle,
  notifications: Bell,
  tokens: Key,
} as const;

const WORKSPACE_TAB_SEGMENTS = [
  ["general", "members", "billing"],
  ["issue"],
  ["repositories", "integrations", "mcp", "plugins", "labs"],
] as const;
type WorkspaceTabKey = (typeof WORKSPACE_TAB_SEGMENTS)[number][number];

const WORKSPACE_TAB_VALUES = {
  general: "workspace",
  members: "members",
  billing: "billing",
  // Not `issue`: that value belongs to the account's Issue tab. Same feature
  // name in both scopes, two distinct panels, so they need distinct values.
  issue: "workspace-issue",
  repositories: "repositories",
  integrations: "integrations",
  mcp: "mcp",
  plugins: "plugins",
  labs: "labs",
} as const satisfies Record<WorkspaceTabKey, string>;

const WORKSPACE_TAB_ICONS = {
  general: Settings,
  members: Users,
  billing: CreditCard,
  issue: ListTodo,
  repositories: FolderGit2,
  integrations: Plug,
  mcp: Server,
  plugins: Blocks,
  labs: FlaskConical,
} as const satisfies Record<WorkspaceTabKey, React.ComponentType<{ className?: string }>>;

const WORKSPACE_TAB_LABEL_KEYS = {
  general: "general",
  members: "members",
  billing: "billing",
  issue: "workspace_issue",
  repositories: "repositories",
  integrations: "integrations",
  mcp: "mcp",
  plugins: "plugins",
  labs: "labs",
} as const satisfies Record<WorkspaceTabKey, string>;

const DEFAULT_TAB = "profile";
const TAB_QUERY_KEY = "tab";

// Legacy `?tab=…` values that have been collapsed into another tab. Old
// bookmarks and in-app deep links still land on the correct surface without us
// preserving a dead TabsContent entry. Lark used to be its own top-level
// workspace tab and now lives inside Integrations; MUL-6232 folded GitHub and
// the self-hosted providers into Repositories, Composio into MCP, and
// Labels / Properties / Quick Actions into the workspace Issue tab, which
// reads the matching section from the same query string.
const LEGACY_WORKSPACE_TAB_REDIRECTS: Record<string, string> = {
  lark: "integrations",
  github: "repositories",
  composio: "mcp",
  labels: "workspace-issue",
  properties: "workspace-issue",
  "quick-actions": "workspace-issue",
};

const SETTINGS_TAB_TRIGGER_CLASS =
  "h-8 shrink-0 px-2.5 hover:bg-surface-hover data-active:!bg-surface-selected data-active:!text-surface-selected-foreground data-active:hover:!bg-surface-selected md:!w-full md:px-2 md:after:hidden";

export interface DesktopSettingsTab {
  value: string;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  content: React.ReactNode;
}

interface SettingsPageProps {
  /**
   * Desktop-only tabs, rendered as their own "Desktop (This Device)" group.
   * The bar for this group is narrow on purpose: only settings that exist
   * *because* this is the desktop app. Theme and shortcuts are stored locally
   * too, but the web app has them as well, so they stay under Account.
   */
  desktopTabs?: DesktopSettingsTab[];
}

export function SettingsPage({ desktopTabs }: SettingsPageProps = {}) {
  const { t } = useT("settings");
  const workspaceName = useCurrentWorkspace()?.name;
  const navigation = useNavigation();
  const isMobile = useIsMobile();
  const pluginsEnabled = useFeatureEnabled(PLUGINS_V1_FLAG, false);
  const billingEnabled = useFeatureEnabled(
    BILLING_WORKSPACE_SUBSCRIPTIONS_FLAG,
    false,
  );

  const visibleWorkspaceSegments = React.useMemo(
    () =>
      WORKSPACE_TAB_SEGMENTS.map((segment) =>
        segment.filter(
          (key) =>
            (key !== "plugins" || pluginsEnabled) &&
            (key !== "billing" || billingEnabled) &&
            (key !== "labs" || LABS_HAS_EXPERIMENTS),
        ),
      ).filter((segment) => segment.length > 0),
    [billingEnabled, pluginsEnabled],
  );
  const visibleWorkspaceTabKeys = React.useMemo(
    () => visibleWorkspaceSegments.flat(),
    [visibleWorkspaceSegments],
  );

  // Whitelist of valid tab values; unknown ?tab=… values silently fall back to
  // the default. Whitelisting also blocks junk like ?tab=<script> from
  // surfacing in the DOM via Radix Tabs internals.
  const validTabs = React.useMemo(
    () =>
      new Set<string>([
        ...ACCOUNT_TAB_KEYS,
        ...visibleWorkspaceTabKeys.map((key) => WORKSPACE_TAB_VALUES[key]),
        ...(desktopTabs?.map((tab) => tab.value) ?? []),
      ]),
    [desktopTabs, visibleWorkspaceTabKeys],
  );

  const tabFromUrl = navigation.searchParams.get(TAB_QUERY_KEY);
  const candidateTab = tabFromUrl
    ? tabFromUrl === "billing" && !billingEnabled
      ? "workspace"
      : LEGACY_WORKSPACE_TAB_REDIRECTS[tabFromUrl] ?? tabFromUrl
    : null;
  const activeTab =
    candidateTab && validTabs.has(candidateTab) ? candidateTab : DEFAULT_TAB;

  // replace (not push) so settings tab switches don't pollute browser history.
  // Preserve any other query params the page may carry.
  const handleTabChange = (next: string) => {
    const params = new URLSearchParams(navigation.searchParams);
    params.set(TAB_QUERY_KEY, next);
    navigation.replace(`${navigation.pathname}?${params.toString()}`);
  };

  const renderTrigger = (
    value: string,
    label: string,
    Icon: React.ComponentType<{ className?: string }>,
    className?: string,
  ) => (
    <TabsTrigger
      key={value}
      value={value}
      className={cn(SETTINGS_TAB_TRIGGER_CLASS, className)}
    >
      <Icon className="h-4 w-4" />
      {label}
    </TabsTrigger>
  );

  return (
    <Tabs
      value={activeTab}
      onValueChange={handleTabChange}
      orientation={isMobile ? "horizontal" : "vertical"}
      className="flex flex-1 min-h-0 flex-col gap-0 overflow-y-auto md:flex-row md:overflow-hidden"
    >
      {/* Structural navigation; bounded setting groups remain in the content surface.
          Stays on the content surface color (no shell tint): the desktop's active
          tab merges into the card top, and a tinted panel under the first tabs
          breaks that seam (MUL-4439). Zoning comes from the divider instead. */}
      <div className="shrink-0 overflow-x-auto border-b border-surface-border p-2 md:w-56 md:overflow-y-auto md:border-b-0 md:border-r md:p-4">
        {/* This page builds its own chrome instead of a PageHeader, so it has
            to supply the nav trigger itself — below `xl` the nav is a sheet or
            auto-collapsed, and settings has no other way back to it. */}
        {/* The gap below this row belongs to the row, not to the heading: with
            `items-center`, a bottom margin on the `h1` is part of the box being
            centred, so it offsets the heading against the trigger beside it. */}
        <div className="flex items-center md:mb-4">
          <CollapsedNavTrigger />
          <h1 className="sr-only text-body font-semibold md:not-sr-only md:px-2">{t(($) => $.page.title)}</h1>
        </div>
        <TabsList
          variant="line"
          className="flex w-max min-w-full flex-row items-center gap-1 p-0 md:w-full md:flex-col md:items-stretch"
        >
          {/* Account group */}
          <span className="hidden px-2 pb-1 pt-2 text-caption font-medium text-muted-foreground md:block">
            {t(($) => $.page.groups.account)}
          </span>
          {ACCOUNT_TAB_KEYS.map((key) =>
            renderTrigger(key, t(($) => $.page.tabs[key]), ACCOUNT_TAB_ICONS[key]),
          )}

          {/* Desktop group — absent on web, where the app injects no tabs */}
          {desktopTabs?.length ? (
            <>
              <span className="hidden px-2 pb-1 pt-4 text-caption font-medium text-muted-foreground md:block">
                {t(($) => $.page.groups.desktop)}
              </span>
              {desktopTabs.map((tab) => renderTrigger(tab.value, tab.label, tab.icon))}
            </>
          ) : null}

          {/* Workspace group */}
          <span className="hidden truncate px-2 pb-1 pt-4 text-caption font-medium text-muted-foreground md:block">
            {workspaceName ?? t(($) => $.page.workspace_fallback)}
          </span>
          {visibleWorkspaceSegments.map((segment, index) => (
            <React.Fragment key={segment.join("-")}>
              {segment.map((key, keyIndex) =>
                renderTrigger(
                  WORKSPACE_TAB_VALUES[key],
                  t(($) => $.page.tabs[WORKSPACE_TAB_LABEL_KEYS[key]]),
                  WORKSPACE_TAB_ICONS[key],
                  // Segment break: extra space on the first row of each
                  // segment after the first. A rule here fenced single-entry
                  // segments on both sides, which reads as separation rather
                  // than grouping — spacing groups just as well and adds no
                  // chrome. Vertical only; at mobile widths this list is one
                  // scrolling row, where the gap belongs between columns.
                  index > 0 && keyIndex === 0 ? "md:mt-3" : undefined,
                ),
              )}
            </React.Fragment>
          ))}
        </TabsList>
      </div>

      {/* Right content */}
      <div className="min-w-0 flex-1 md:overflow-y-auto">
        {/* The workspace Issue tab is the one management surface here — search,
            table, row actions — so it gets the wider measure; every other tab
            is a form and reads better narrow. */}
        <div
          className={`mx-auto w-full p-4 sm:p-6 md:p-8 ${
            activeTab === WORKSPACE_TAB_VALUES.issue ? "max-w-5xl" : "max-w-3xl"
          }`}
        >
          <TabsContent value="profile"><AccountTab /></TabsContent>
          <TabsContent value="preferences"><PreferencesTab /></TabsContent>
          <TabsContent value="shortcuts"><KeyboardShortcutsTab /></TabsContent>
          <TabsContent value="issue"><IssueTab /></TabsContent>
          <TabsContent value="chat"><ChatTab /></TabsContent>
          <TabsContent value="notifications"><NotificationsTab /></TabsContent>
          <TabsContent value="tokens"><TokensTab /></TabsContent>
          {desktopTabs?.map((tab) => (
            <TabsContent key={tab.value} value={tab.value}>{tab.content}</TabsContent>
          ))}
          <TabsContent value="workspace"><WorkspaceTab /></TabsContent>
          <TabsContent value="members"><MembersTab /></TabsContent>
          {billingEnabled ? (
            <TabsContent value="billing"><BillingTab /></TabsContent>
          ) : null}
          <TabsContent value="workspace-issue"><WorkspaceIssueTab /></TabsContent>
          <TabsContent value="repositories"><RepositoriesTab /></TabsContent>
          <TabsContent value="integrations"><IntegrationsTab /></TabsContent>
          <TabsContent value="mcp"><McpTab /></TabsContent>
          {pluginsEnabled ? <TabsContent value="plugins"><PluginsTab /></TabsContent> : null}
          {LABS_HAS_EXPERIMENTS ? (
            <TabsContent value="labs"><LabsTab /></TabsContent>
          ) : null}
        </div>
      </div>
    </Tabs>
  );
}
