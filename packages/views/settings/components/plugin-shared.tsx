"use client";

import type {
  PluginBindingRequest,
  PluginInstallation,
  PluginRemoteMCPConfig,
  RemoteMCPDiscoveryResponse,
} from "@multica/core/types";
import { Badge } from "@multica/ui/components/ui/badge";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

/** First-letter logo used across the Plugins list, marketplace, and detail views. */
export function PluginLogo({ name, className }: { name: string; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn(
        "flex size-8 shrink-0 select-none items-center justify-center rounded-lg bg-muted text-body font-semibold text-muted-foreground",
        className,
      )}
    >
      {(name.trim().charAt(0) || "#").toUpperCase()}
    </span>
  );
}

export type InstallationState = "disabled" | "activating" | "healthy" | "degraded" | "failed";

export function installationState(installation: PluginInstallation): InstallationState {
  if (installation.enabled !== true) return "disabled";
  if (installation.lifecycle_status === "activating") return "activating";
  if (installation.health_state === "error" || installation.lifecycle_status === "error") return "failed";
  if (installation.health_state === "degraded" || installation.lifecycle_status === "degraded") return "degraded";
  return "healthy";
}

/** Enabled, but at least one remote MCP contribution is not ready yet. */
export function installationNeedsSetup(installation: PluginInstallation): boolean {
  return installation.enabled === true
    && !installation.remote_mcp.every((config) => config.ready === true);
}

const STATE_DOT_CLASSES: Record<InstallationState, string> = {
  disabled: "bg-faint-foreground",
  activating: "animate-pulse bg-muted-foreground",
  healthy: "bg-success",
  degraded: "bg-warning",
  failed: "bg-destructive",
};

/** Dot + label health summary for an installation (list row and detail header). */
export function InstallationHealth({ installation }: { installation: PluginInstallation }) {
  const { t } = useT("settings");
  const state = installationState(installation);
  const label = state === "disabled"
    ? t(($) => $.plugins.turned_off)
    : state === "activating"
      ? t(($) => $.plugins.states.activating)
      : state === "degraded"
        ? t(($) => $.plugins.states.degraded)
        : state === "failed"
          ? t(($) => $.plugins.states.failed)
          : t(($) => $.plugins.states.healthy);
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap text-caption text-muted-foreground">
      <span aria-hidden className={cn("size-1.5 rounded-full", STATE_DOT_CLASSES[state])} />
      {label}
    </span>
  );
}

export function NeedsSetupBadge() {
  const { t } = useT("settings");
  return (
    <Badge variant="secondary" className="border-transparent bg-warning/15 text-foreground">
      {t(($) => $.plugins.needs_setup)}
    </Badge>
  );
}

type SetEnabledInput = {
  installationId: string;
  enabled: boolean;
  binding: PluginBindingRequest;
};

/**
 * Workspace-level on/off semantics shared by the list row and the detail
 * header switch. ON enables the workspace binding; OFF disables every
 * currently-enabled binding sequentially so no scope survives the off state.
 */
export async function applyInstallationEnabled(
  setEnabled: (input: SetEnabledInput) => Promise<unknown>,
  installation: PluginInstallation,
  wsId: string,
  enabled: boolean,
): Promise<void> {
  if (enabled) {
    await setEnabled({
      installationId: installation.id,
      enabled: true,
      binding: { scope_type: "workspace", scope_id: wsId },
    });
    return;
  }
  for (const binding of installation.bindings) {
    if (binding.enabled !== true) continue;
    await setEnabled({
      installationId: installation.id,
      enabled: false,
      binding: {
        scope_type: binding.scope_type === "agent" ? "agent" : "workspace",
        scope_id: binding.scope_id,
      },
    });
  }
}

export function isRemoteMCPConnected(config: PluginRemoteMCPConfig): boolean {
  return Boolean(config.config_revision)
    && (config.credential_state === "configured" || config.credential_state === "not_required");
}

/** Discovery awaiting review, reconstructed from the stored config. */
export function pendingDiscovery(config: PluginRemoteMCPConfig): RemoteMCPDiscoveryResponse | null {
  if (config.reviewed === true || config.discovered_tools.length === 0) return null;
  return {
    config_revision: config.config_revision ?? 0,
    discovered_tools: config.discovered_tools,
    discovered_schema_digest: config.discovered_schema_digest ?? "",
  };
}
