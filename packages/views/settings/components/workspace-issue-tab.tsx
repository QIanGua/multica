"use client";

import { Button } from "@multica/ui/components/ui/button";
import { cn } from "@multica/ui/lib/utils";
import { useNavigation } from "../../navigation";
import { useT } from "../../i18n";
import { SettingsTab } from "./settings-layout";
import { PropertiesPanel } from "./properties-tab";
import { LabelsPanel } from "./labels-tab";
import { QuickActionsPanel } from "./quick-actions-tab";

/**
 * Workspace-level issue configuration: properties, labels, and quick actions.
 *
 * These three were separate top-level tabs, which put three entries in the nav
 * for one subject and left no room to say what the subject was. They are all
 * "configure once, applies to every issue in this workspace", so they belong on
 * one page behind in-page tabs (MUL-6232).
 *
 * Its twin under Account — `issue-tab.tsx` — is *this person's* issue
 * preferences. Same name, two scopes; the group heading in the nav carries the
 * distinction, and each page points at the other.
 */
const SECTIONS = ["properties", "labels", "quick_actions"] as const;
type IssueSection = (typeof SECTIONS)[number];

const SECTION_QUERY_KEY = "section";
const SECTION_VALUES = {
  properties: "properties",
  labels: "labels",
  quick_actions: "quick-actions",
} as const satisfies Record<IssueSection, string>;

// Doubles as the legacy `?tab=` map: each section used to be its own tab under
// exactly this value, so an old bookmark — which SettingsPage redirects here —
// still names the panel it meant.
const SECTION_BY_VALUE: Record<string, IssueSection> = {
  properties: "properties",
  labels: "labels",
  "quick-actions": "quick_actions",
};

const DEFAULT_SECTION: IssueSection = "properties";

export function WorkspaceIssueTab() {
  const { t } = useT("settings");
  const navigation = useNavigation();

  const sectionParam = navigation.searchParams.get(SECTION_QUERY_KEY) ?? "";
  const legacyTab = navigation.searchParams.get("tab") ?? "";
  const section: IssueSection =
    SECTION_BY_VALUE[sectionParam] ??
    SECTION_BY_VALUE[legacyTab] ??
    DEFAULT_SECTION;

  const handleSectionChange = (next: IssueSection) => {
    const params = new URLSearchParams(navigation.searchParams);
    // Rewrite `tab` too: arriving from a legacy value leaves it pointing at the
    // old surface, and the section param alone would then be ignored on reload.
    params.set("tab", "workspace-issue");
    params.set(SECTION_QUERY_KEY, SECTION_VALUES[next]);
    navigation.replace(`${navigation.pathname}?${params.toString()}`);
  };

  const sectionLabels: Record<IssueSection, string> = {
    properties: t(($) => $.properties.title),
    labels: t(($) => $.labels.title),
    quick_actions: t(($) => $.quick_actions.title),
  };
  // Each panel used to be a page and carried its own description. They still
  // need one — "labels come in two independent sets" is not something the UI
  // shows on its own — so it sits under the strip, scoped to the open section.
  const sectionDescriptions: Record<IssueSection, string> = {
    properties: t(($) => $.properties.description),
    labels: t(($) => $.labels.description),
    quick_actions: t(($) => $.quick_actions.description),
  };

  return (
    <SettingsTab
      title={t(($) => $.page.tabs.workspace_issue)}
      description={t(($) => $.workspace_issue.description)}
    >
      <div className="space-y-5">
        <div className="flex flex-wrap items-center gap-1" role="tablist">
          {SECTIONS.map((key) => {
            const active = key === section;
            return (
              <Button
                key={key}
                type="button"
                role="tab"
                aria-selected={active}
                size="sm"
                variant={active ? "secondary" : "ghost"}
                className={cn(
                  active &&
                    "bg-surface-selected text-surface-selected-foreground hover:bg-surface-selected",
                )}
                onClick={() => handleSectionChange(key)}
              >
                {sectionLabels[key]}
              </Button>
            );
          })}
        </div>

        <p className="px-0.5 text-caption leading-5 text-muted-foreground">
          {sectionDescriptions[section]}
        </p>

        {section === "properties" ? <PropertiesPanel /> : null}
        {section === "labels" ? <LabelsPanel /> : null}
        {section === "quick_actions" ? <QuickActionsPanel /> : null}
      </div>
    </SettingsTab>
  );
}
