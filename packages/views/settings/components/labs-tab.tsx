"use client";

import { FlaskConical } from "lucide-react";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@multica/ui/components/ui/empty";
import { useT } from "../../i18n";
import { SettingsCard, SettingsTab } from "./settings-layout";

// The Co-authored-by trailer toggle moved in with the rest of the GitHub
// settings (see github-settings.tsx). Labs stays as the container for future
// experimental flags rather than being removed from the IA.

/**
 * Whether Labs has anything in it. There are no experiments today, so the
 * settings nav omits the entry entirely rather than offering a route to an
 * empty page (MUL-6232). Add the first experiment below and flip this.
 *
 * Typed `boolean` rather than left as the `false` literal so the tab it gates
 * does not read as statically unreachable.
 */
export const LABS_HAS_EXPERIMENTS: boolean = false;

export function LabsTab() {
  const { t } = useT("settings");
  return (
    <SettingsTab title={t(($) => $.page.tabs.labs)}>
      <SettingsCard>
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <FlaskConical className="h-4 w-4" />
            </EmptyMedia>
            <EmptyTitle>{t(($) => $.labs.section_placeholder_title)}</EmptyTitle>
            <EmptyDescription>
              {t(($) => $.labs.section_placeholder_description)}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      </SettingsCard>
    </SettingsTab>
  );
}
