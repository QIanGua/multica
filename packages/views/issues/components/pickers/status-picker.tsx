"use client";

import { useMemo, useState } from "react";
import type { IssueStatus, UpdateIssueRequest } from "@multica/core/types";
import { ALL_STATUSES, STATUS_CONFIG } from "@multica/core/issues/config";
import { useIssueStatuses } from "@multica/core/issue-statuses/hooks";
import { useWorkspaceId } from "@multica/core/hooks";
import { StatusIcon } from "../status-icon";
import { PropertyPicker, PickerItem, PickerGroupLabel } from "./property-picker";
import { useT } from "../../../i18n";
import { useStatusLabel } from "../../utils/status-label";

/** Above this many options the flat list stops being scannable. */
const SEARCH_THRESHOLD = 9;

export function StatusPicker({
  status,
  onUpdate,
  trigger: customTrigger,
  triggerRender,
  open: controlledOpen,
  onOpenChange: controlledOnOpenChange,
  align,
}: {
  /**
   * The currently-selected status, used to check the matching row. `null`
   * means "no single current value" (e.g. a batch selection spanning several
   * statuses) — no row is checked. Single-issue callers always pass a concrete
   * status.
   */
  status: IssueStatus | null;
  onUpdate: (updates: Partial<UpdateIssueRequest>) => void;
  trigger?: React.ReactNode;
  triggerRender?: React.ReactElement;
  open?: boolean;
  onOpenChange?: (v: boolean) => void;
  align?: "start" | "center" | "end";
}) {
  const [internalOpen, setInternalOpen] = useState(false);
  const open = controlledOpen ?? internalOpen;
  const setOpen = controlledOnOpenChange ?? setInternalOpen;
  const [query, setQuery] = useState("");
  const { t } = useT("issues");
  // Every StatusPicker call site lives inside the workspace shell (issue
  // detail, table, board batch toolbar, create-issue modal), so the provider
  // is guaranteed here.
  const wsId = useWorkspaceId();
  const { activeStatuses, categoryOf, entryOf } = useIssueStatuses(wsId);
  const labelOf = useStatusLabel(wsId);

  /**
   * Offerable statuses grouped by category, in canonical category order.
   *
   * Archived statuses are excluded: archiving retires a status from future
   * assignment while leaving the issues already on it untouched. Falls back to
   * the 7 built-ins until the catalog lands, so a cold render offers exactly
   * what it always did instead of an empty popover. (MUL-6243)
   */
  const groups = useMemo(() => {
    const q = query.trim().toLowerCase();

    return ALL_STATUSES.map((category) => {
      const entries = activeStatuses.filter((e) => e.category === category);
      const options =
        entries.length > 0
          ? entries.map((e) => ({
              key: e.key as IssueStatus,
              label: labelOf(e.key),
              color: e.is_system ? null : e.color,
            }))
          : // No catalog row for this category yet — the catalog is still in
            // flight, or this pod predates the seed. Offer the built-in, whose
            // key IS the category, so the picker is never short a lifecycle step.
            [{ key: category as IssueStatus, label: labelOf(category), color: null }];
      return {
        category,
        options: q ? options.filter((o) => o.label.toLowerCase().includes(q)) : options,
      };
    }).filter((g) => g.options.length > 0);
  }, [activeStatuses, query, labelOf]);

  const optionCount = groups.reduce((n, g) => n + g.options.length, 0);
  // Category headings only earn their space once a category holds more than
  // one status. A workspace that never customized anything sees the same flat
  // 7-row list as before.
  const showGroupLabels = groups.some((g) => g.options.length > 1);
  const searchable = optionCount > SEARCH_THRESHOLD;

  return (
    <PropertyPicker
      open={open}
      onOpenChange={(v) => {
        if (!v) setQuery("");
        setOpen(v);
      }}
      width="w-52"
      align={align}
      triggerRender={triggerRender}
      searchable={searchable}
      searchPlaceholder={t(($) => $.filters.search_status)}
      onSearchChange={setQuery}
      trigger={
        customTrigger ??
        (status != null ? (
          <>
            <StatusIcon
              status={status}
              category={categoryOf(status)}
              color={entryOf(status)?.color}
              className="h-3.5 w-3.5 shrink-0"
            />
            <span className="truncate">{labelOf(status)}</span>
          </>
        ) : null)
      }
    >
      {groups.map((group) => (
        <div key={group.category}>
          {showGroupLabels && (
            <PickerGroupLabel>{t(($) => $.status[group.category])}</PickerGroupLabel>
          )}
          {group.options.map((option) => (
            <PickerItem
              key={option.key}
              selected={option.key === status}
              hoverClassName={STATUS_CONFIG[group.category].hoverBg}
              onClick={() => {
                onUpdate({ status: option.key });
                setOpen(false);
                setQuery("");
              }}
            >
              <StatusIcon
                status={option.key}
                category={group.category}
                color={option.color}
                className="h-3.5 w-3.5"
              />
              <span className="truncate">{option.label}</span>
            </PickerItem>
          ))}
        </div>
      ))}
    </PropertyPicker>
  );
}
