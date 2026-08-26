"use client";

import { useEffect, useState } from "react";
import { AlertTriangle, Info } from "lucide-react";
import { toast } from "sonner";
import { ApiError } from "@multica/core/api";
import type { Agent, AgentRuntime } from "@multica/core/types";
import { runtimeDisplayLabel } from "@multica/core/runtimes";
import { useRevokeRuntimeAndMakePrivate } from "@multica/core/runtimes/mutations";
import {
  AlertDialog,
  AlertDialogContent,
} from "@multica/ui/components/ui/alert-dialog";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { useT } from "../../i18n";

// RevokeVisibilityDialog is the impact confirmation for taking a shared machine
// back (public → private), MUL-6704.
//
// Reclaiming access is not a display change: agents belonging to other members
// lose their binding, their queued and running work is cancelled, and their
// Autopilots are paused. The plain PATCH therefore refuses with
// `runtime_visibility_has_foreign_agents` and this plan; the user confirms the
// exact set here, and the confirmed endpoint re-checks it under a lock so what
// they approved is what happens.
//
// Shape mirrors DeleteRuntimeDialog's cascade mode on purpose — same 409 → plan
// → checkbox → re-confirm-on-plan-changed flow — because it is the same class of
// decision and users have already learned it.
export interface RuntimeRevokePlan {
  activeAgents: Agent[];
  archivedAgentCount: number;
  retainedAgentCount: number;
  mikaAffected: boolean;
}

export interface RevokeVisibilityDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  runtime: AgentRuntime;
  wsId: string;
  plan: RuntimeRevokePlan;
  // Called after the revoke commits, so the caller can toast and refresh.
  onRevoked: () => void;
}

export function RevokeVisibilityDialog({
  open,
  onOpenChange,
  runtime,
  wsId,
  plan: initialPlan,
  onRevoked,
}: RevokeVisibilityDialogProps) {
  const { t } = useT("runtimes");
  const [plan, setPlan] = useState<RuntimeRevokePlan>(initialPlan);
  const [confirmed, setConfirmed] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [planChangedNotice, setPlanChangedNotice] = useState<string | null>(null);
  const revoke = useRevokeRuntimeAndMakePrivate(wsId);

  // Re-seed on every open: a notice or a ticked checkbox surviving from a
  // previous attempt would let the user confirm a plan they never read.
  useEffect(() => {
    if (open) {
      setPlan(initialPlan);
      setConfirmed(false);
      setSubmitting(false);
      setPlanChangedNotice(null);
    }
  }, [open, initialPlan]);

  const handleConfirm = async () => {
    setSubmitting(true);
    setPlanChangedNotice(null);
    try {
      await revoke.mutateAsync({
        runtimeId: runtime.id,
        expectedActiveAgentIds: plan.activeAgents.map((a) => a.id),
      });
      onRevoked();
    } catch (err) {
      // Someone bound or archived an agent while this dialog was open. The
      // server wrote nothing; show the fresh plan and require a new tick.
      const conflict = parseRuntimeRevokeConflict(err);
      if (conflict?.code === "runtime_visibility_plan_changed") {
        setPlan(conflict.plan);
        setConfirmed(false);
        setPlanChangedNotice(
          t(($) => $.detail.revoke_visibility_dialog.notice_plan_changed),
        );
        return;
      }
      toast.error(
        err instanceof Error && err.message
          ? err.message
          : t(($) => $.detail.revoke_visibility_dialog.failed_toast),
      );
    } finally {
      setSubmitting(false);
    }
  };

  const handleOpenChange = (next: boolean) => {
    if (submitting) return;
    onOpenChange(next);
  };

  const count = plan.activeAgents.length;

  return (
    <AlertDialog open={open} onOpenChange={handleOpenChange}>
      <AlertDialogContent
        className="w-[calc(100vw-2rem)] !max-w-[560px] gap-0 overflow-hidden rounded-lg p-0"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-5 pb-4 pt-5">
          <h2 className="text-title-sm font-semibold">
            {t(($) => $.detail.revoke_visibility_dialog.title, { count })}
          </h2>
          <p className="mt-1 text-body leading-5 text-muted-foreground">
            {t(($) => $.detail.revoke_visibility_dialog.description, {
              name: runtimeDisplayLabel(runtime),
            })}
          </p>

          <div
            role="alert"
            className="mt-3 flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-caption text-destructive"
          >
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
            <span>{t(($) => $.detail.revoke_visibility_dialog.warning)}</span>
          </div>

          {/* The workspace-wide consequence users do not expect: Mika is one
              per workspace, so unbinding her stops her for everyone, not just
              for her owner. */}
          {plan.mikaAffected && (
            <div
              role="alert"
              className="mt-2 flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-caption text-destructive"
            >
              <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
              <span>
                {t(($) => $.detail.revoke_visibility_dialog.mika_warning)}
              </span>
            </div>
          )}

          {planChangedNotice && (
            <div
              role="status"
              className="mt-2 rounded-md border bg-muted/40 px-3 py-2 text-caption text-foreground"
            >
              {planChangedNotice}
            </div>
          )}

          {count > 0 && (
            <ul className="mt-3 max-h-48 divide-y overflow-y-auto rounded-md border">
              {plan.activeAgents.map((agent) => (
                <li
                  key={agent.id}
                  className="flex items-center justify-between gap-2 px-3 py-2 text-body"
                >
                  <span className="truncate">{agent.name}</span>
                  <span className="shrink-0 text-caption text-muted-foreground">
                    {t(($) => $.detail.revoke_visibility_dialog.agent_action_unbind)}
                  </span>
                </li>
              ))}
            </ul>
          )}

          {plan.archivedAgentCount > 0 && (
            <p className="mt-2 text-caption text-muted-foreground">
              {t(($) => $.detail.revoke_visibility_dialog.archived_note, {
                count: plan.archivedAgentCount,
              })}
            </p>
          )}

          {/* Builder carriers keep their binding — unbinding one strands a row
              with no UI to repair, deleting it destroys the conversation — but
              they cannot run here any more, so the copy points at the fix. */}
          {plan.retainedAgentCount > 0 && (
            <div
              role="status"
              className="mt-2 flex items-start gap-2 rounded-md border border-warning/40 bg-warning/5 px-3 py-2 text-caption"
            >
              <Info className="mt-0.5 size-3.5 shrink-0 text-warning" />
              <span>
                {t(($) => $.detail.revoke_visibility_dialog.retained_note, {
                  count: plan.retainedAgentCount,
                })}
              </span>
            </div>
          )}

          <label className="mt-4 flex cursor-pointer items-start gap-2 text-body">
            <Checkbox
              checked={confirmed}
              onCheckedChange={(next) => setConfirmed(next === true)}
              disabled={submitting}
            />
            <span>
              {t(($) => $.detail.revoke_visibility_dialog.checkbox, { count })}
            </span>
          </label>
        </div>

        <div className="border-t bg-muted/25 px-5 py-3">
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button
              type="button"
              variant="outline"
              className="w-full sm:w-auto"
              onClick={() => handleOpenChange(false)}
              disabled={submitting}
            >
              {t(($) => $.detail.revoke_visibility_dialog.cancel)}
            </Button>
            <Button
              type="button"
              variant="destructive"
              className="w-full sm:w-auto"
              onClick={handleConfirm}
              disabled={submitting || !confirmed}
            >
              {submitting
                ? t(($) => $.detail.revoke_visibility_dialog.submitting)
                : t(($) => $.detail.revoke_visibility_dialog.confirm)}
            </Button>
          </div>
        </div>
      </AlertDialogContent>
    </AlertDialog>
  );
}

export interface RuntimeRevokeConflict {
  code:
    | "runtime_visibility_has_foreign_agents"
    | "runtime_visibility_plan_changed";
  plan: RuntimeRevokePlan;
}

/**
 * Reads the structured 409 the visibility endpoints return. Anything else — a
 * different status, a different code, a missing body — collapses to `null` so
 * callers fall through to their generic error handling and never open a
 * confirmation dialog on a plan they did not receive.
 */
export function parseRuntimeRevokeConflict(
  err: unknown,
): RuntimeRevokeConflict | null {
  if (!(err instanceof ApiError)) return null;
  if (err.status !== 409) return null;
  const body = err.body;
  if (!body || typeof body !== "object") return null;
  const record = body as Record<string, unknown>;
  const code = record.code;
  if (
    code !== "runtime_visibility_has_foreign_agents" &&
    code !== "runtime_visibility_plan_changed"
  ) {
    return null;
  }
  const rawAgents = record.active_agents;
  const activeAgents = Array.isArray(rawAgents)
    ? rawAgents.filter(
        (a): a is Agent =>
          typeof a === "object" &&
          a !== null &&
          typeof (a as Record<string, unknown>).id === "string" &&
          typeof (a as Record<string, unknown>).name === "string",
      )
    : [];
  return {
    code,
    plan: {
      activeAgents,
      archivedAgentCount: numberOrZero(record.archived_agent_count),
      retainedAgentCount: numberOrZero(record.retained_agent_count),
      mikaAffected: record.mika_affected === true,
    },
  };
}

function numberOrZero(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}
