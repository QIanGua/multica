"use client";

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { IssueProperty, IssuePropertyValue } from "@multica/core/types";
import {
  actorRefsFromValue,
  formatActorRef,
  MAX_ISSUE_PROPERTY_ACTOR_VALUES,
} from "@multica/core/types";
import { memberListOptions, agentListOptions } from "@multica/core/workspace/queries";
import { useActorName } from "@multica/core/workspace/hooks";
import { useWorkspaceId } from "@multica/core/hooks";
import { ActorAvatar } from "../../../common/actor-avatar";
import { useT } from "../../../i18n";
import { matchesPinyin } from "../../../editor/extensions/pinyin-match";
import { PropertyPicker, PickerItem, PickerSection, PickerEmpty } from "./property-picker";

/**
 * Value editor for `actor` / `multi_actor` custom properties (MUL-6286).
 *
 * Shaped like AssigneePicker — same member/agent sections, same avatars — with
 * two deliberate differences:
 *
 *   1. No squad section. A squad is a routing target, not a person; the actor
 *      property's value range stops at member/agent.
 *   2. Agents are never disabled for runtime binding or invoke permission.
 *      Writing an agent here is a REFERENCE, not an assignment: nothing is
 *      enqueued, so "can this agent run" is the wrong question. The server
 *      gates the same way — on visibility, not invocability.
 *
 * `multi_actor` toggles in place and keeps the popover open (mirroring
 * multi_select); `actor` commits and closes.
 */
export function ActorPropertyPicker({
  property,
  value,
  onChange,
  open,
  onOpenChange,
  trigger,
  triggerRender,
  emptyRow,
}: {
  property: IssueProperty;
  value: IssuePropertyValue | undefined;
  onChange: (value: IssuePropertyValue | undefined) => void;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  trigger: React.ReactNode;
  triggerRender?: React.ReactElement<Record<string, unknown>>;
  emptyRow: React.ReactNode;
}) {
  const { t } = useT("issues");
  const [filter, setFilter] = useState("");
  const wsId = useWorkspaceId();
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const { data: agents = [] } = useQuery(agentListOptions(wsId));

  const multiple = property.type === "multi_actor";
  const selected = actorRefsFromValue(value);
  const selectedKeys = new Set(selected.map((ref) => formatActorRef(ref.kind, ref.id)));
  const atCapacity = multiple && selected.length >= MAX_ISSUE_PROPERTY_ACTOR_VALUES;

  const query = filter.trim().toLowerCase();
  const matches = (name: string) => name.toLowerCase().includes(query) || matchesPinyin(name, query);
  const filteredMembers = members.filter((m) => matches(m.name));
  const filteredAgents = agents.filter((a) => !a.archived_at && matches(a.name));

  const commit = (kind: "member" | "agent", id: string) => {
    const key = formatActorRef(kind, id);
    if (!multiple) {
      onChange(key);
      onOpenChange(false);
      return;
    }
    // Toggle, preserving insertion order — the stored order is the order the
    // user built, and the server keeps it rather than canonicalizing.
    if (selectedKeys.has(key)) {
      const next = selected
        .map((ref) => formatActorRef(ref.kind, ref.id))
        .filter((existing) => existing !== key);
      if (next.length === 0) onChange(undefined);
      else onChange(next);
      return;
    }
    if (atCapacity) return;
    onChange([...selected.map((ref) => formatActorRef(ref.kind, ref.id)), key]);
  };

  const rowsEmpty = filteredMembers.length === 0 && filteredAgents.length === 0;

  return (
    <PropertyPicker
      open={open}
      onOpenChange={(next: boolean) => {
        onOpenChange(next);
        if (!next) setFilter("");
      }}
      width="w-64"
      align="start"
      searchable
      searchPlaceholder={t(($) => $.pickers.custom_property.actor_search_placeholder)}
      onSearchChange={setFilter}
      trigger={trigger}
      triggerRender={triggerRender}
    >
      {emptyRow}

      {filteredMembers.length > 0 && (
        <PickerSection label={t(($) => $.pickers.assignee.members_group)}>
          {filteredMembers.map((m) => {
            const key = formatActorRef("member", m.user_id);
            const isSelected = selectedKeys.has(key);
            return (
              <PickerItem
                key={m.user_id}
                selected={isSelected}
                disabled={atCapacity && !isSelected}
                onClick={() => commit("member", m.user_id)}
              >
                <ActorAvatar actorType="member" actorId={m.user_id} size="sm" />
                <span className="truncate">{m.name}</span>
              </PickerItem>
            );
          })}
        </PickerSection>
      )}

      {filteredAgents.length > 0 && (
        <PickerSection label={t(($) => $.pickers.assignee.agents_group)}>
          {filteredAgents.map((a) => {
            const key = formatActorRef("agent", a.id);
            const isSelected = selectedKeys.has(key);
            return (
              <PickerItem
                key={a.id}
                selected={isSelected}
                disabled={atCapacity && !isSelected}
                onClick={() => commit("agent", a.id)}
              >
                <ActorAvatar actorType="agent" actorId={a.id} size="sm" />
                <span className="truncate">{a.name}</span>
              </PickerItem>
            );
          })}
        </PickerSection>
      )}

      {rowsEmpty && <PickerEmpty />}
    </PropertyPicker>
  );
}

/**
 * Read-only rendering of an actor property value: avatar + name per reference.
 * Falls back to the empty label when nothing resolves, so a value referencing
 * a member who has since left the workspace degrades to the same shape as an
 * unset property rather than rendering a dangling id.
 */
export function ActorPropertyDisplay({
  value,
  emptyLabel,
}: {
  value: IssuePropertyValue | undefined;
  emptyLabel: React.ReactNode;
}) {
  const { getActorName } = useActorName();
  const refs = actorRefsFromValue(value);
  if (refs.length === 0) return <>{emptyLabel}</>;
  return (
    <span className="flex min-w-0 flex-wrap items-center gap-1">
      {refs.map((ref) => (
        <span
          key={formatActorRef(ref.kind, ref.id)}
          className="inline-flex max-w-32 items-center gap-1"
        >
          <ActorAvatar actorType={ref.kind} actorId={ref.id} size="sm" />
          <span className="truncate">{getActorName(ref.kind, ref.id)}</span>
        </span>
      ))}
    </span>
  );
}
