"use client";

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertCircle, ChevronRight, ShieldCheck, Store } from "lucide-react";
import { toast } from "sonner";
import { agentListOptions, memberListOptions } from "@multica/core/workspace/queries";
import { useCurrentMember } from "@multica/core/permissions";
import {
  comparePluginVersions,
  pluginCatalogOptions,
  pluginInstallationsOptions,
  useSetPluginEnabled,
} from "@multica/core/plugins";
import { useCurrentWorkspace } from "@multica/core/paths";
import type { PluginInstallation } from "@multica/core/types";
import { Alert, AlertDescription, AlertTitle } from "@multica/ui/components/ui/alert";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { Switch } from "@multica/ui/components/ui/switch";
import { useT } from "../../i18n";
import { PluginDetail } from "./plugin-detail";
import { PluginMarketplace } from "./plugin-marketplace";
import {
  applyInstallationEnabled,
  InstallationHealth,
  installationNeedsSetup,
  NeedsSetupBadge,
  PluginLogo,
} from "./plugin-shared";
import { SettingsCard, SettingsTab } from "./settings-layout";

type PluginsView = "list" | "marketplace" | { detail: string };

function InstallationRow({
  installation,
  canManage,
  busy,
  onOpen,
  onToggle,
}: {
  installation: PluginInstallation;
  canManage: boolean;
  busy: boolean;
  onOpen: () => void;
  onToggle: (enabled: boolean) => void;
}) {
  const { t } = useT("settings");
  const isPrivate = installation.source_kind === "private_dev";
  const needsSetup = installationNeedsSetup(installation);
  return (
    <div
      role="button"
      tabIndex={0}
      className="flex cursor-pointer items-center gap-3 px-4 py-3.5 outline-none transition-colors hover:bg-muted/50 focus-visible:bg-muted/50"
      onClick={onOpen}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onOpen();
        }
      }}
    >
      <PluginLogo name={installation.display_name} />
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-body font-medium">{installation.display_name}</span>
          <Badge variant="outline">
            {isPrivate ? t(($) => $.plugins.private) : t(($) => $.plugins.official)}
          </Badge>
          {isPrivate ? <Badge variant="destructive">{t(($) => $.plugins.unverified)}</Badge> : null}
        </div>
        <div className="mt-0.5 truncate text-caption text-muted-foreground">
          {installation.description ? <>{installation.description}{" · "}</> : null}
          {"v"}{installation.desired_version}
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-3">
        {needsSetup ? (
          <>
            <NeedsSetupBadge />
            <Button
              size="sm"
              variant="outline"
              onClick={(event) => {
                event.stopPropagation();
                onOpen();
              }}
            >
              {t(($) => $.plugins.finish_setup)}
            </Button>
          </>
        ) : (
          <InstallationHealth installation={installation} />
        )}
        {/* Base UI's Switch re-dispatches the click on a hidden sibling
            input, so a handler on the Switch root alone can't keep the
            toggle from bubbling into the row; the wrapper catches both. */}
        <span
          className="inline-flex"
          onClick={(event) => event.stopPropagation()}
          onKeyDown={(event) => event.stopPropagation()}
        >
          <Switch
            checked={installation.enabled === true}
            disabled={!canManage || busy}
            aria-label={installation.display_name}
            onCheckedChange={(checked) => onToggle(checked)}
          />
        </span>
        <ChevronRight aria-hidden className="size-4 text-faint-foreground" />
      </div>
    </div>
  );
}

export function PluginsTab() {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const wsId = workspace?.id ?? "";
  const currentMember = useCurrentMember(wsId);
  const canManage = currentMember.role === "owner" || currentMember.role === "admin";
  const catalogQuery = useQuery(pluginCatalogOptions(wsId));
  const installationsQuery = useQuery(pluginInstallationsOptions(wsId));
  const refetchInstallations = installationsQuery.refetch;
  const agentsQuery = useQuery(agentListOptions(wsId));
  const membersQuery = useQuery(memberListOptions(wsId));
  const enabledMutation = useSetPluginEnabled(wsId);
  const [view, setView] = useState<PluginsView>("list");

  // OAuth return: consume the callback query params, refresh installations,
  // and jump straight into the detail view of the installation whose
  // discovery is now waiting for review (the detail view auto-opens the
  // review dialog from there).
  useEffect(() => {
    const search = new URLSearchParams(window.location.search);
    const connected = search.get("remote_mcp_connected") === "1";
    const failed = search.has("remote_mcp_error");
    if (!connected && !failed) return;

    if (connected) {
      toast.success(t(($) => $.plugins.remote_mcp.oauth_connected_success));
      void Promise.resolve(refetchInstallations()).then((result) => {
        const plugins = result?.data?.plugins ?? [];
        const affected = plugins.find((installation) => installation.remote_mcp.some(
          (config) => config.reviewed !== true && config.discovered_tools.length > 0,
        ));
        if (affected) setView({ detail: affected.id });
      });
    } else {
      toast.error(t(($) => $.plugins.remote_mcp.oauth_connect_failed));
    }
    search.delete("remote_mcp_connected");
    search.delete("remote_mcp_error");
    const query = search.toString();
    window.history.replaceState(window.history.state, "", `${window.location.pathname}${query ? `?${query}` : ""}${window.location.hash}`);
  }, [refetchInstallations, t]);

  const reportError = (error: unknown) => {
    toast.error(error instanceof Error ? error.message : t(($) => $.plugins.action_failed));
  };

  if (catalogQuery.isPending || installationsQuery.isPending) {
    return (
      <SettingsTab title={t(($) => $.plugins.title)} description={t(($) => $.plugins.description)}>
        <SettingsCard>
          <div className="space-y-3 p-4" aria-label={t(($) => $.plugins.loading)}>
            <Skeleton className="h-5 w-48" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-8 w-28" />
          </div>
        </SettingsCard>
      </SettingsTab>
    );
  }

  if (catalogQuery.isError || installationsQuery.isError) {
    return (
      <SettingsTab title={t(($) => $.plugins.title)} description={t(($) => $.plugins.description)}>
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>{t(($) => $.plugins.load_failed)}</AlertTitle>
          <AlertDescription>{t(($) => $.plugins.load_failed_description)}</AlertDescription>
        </Alert>
      </SettingsTab>
    );
  }

  if (catalogQuery.data?.supported !== true) {
    return (
      <SettingsTab title={t(($) => $.plugins.title)} description={t(($) => $.plugins.description)}>
        <Alert>
          <AlertCircle />
          <AlertTitle>{t(($) => $.plugins.backend_unavailable)}</AlertTitle>
          <AlertDescription>{t(($) => $.plugins.backend_unavailable_description)}</AlertDescription>
        </Alert>
      </SettingsTab>
    );
  }

  const installations = installationsQuery.data?.plugins ?? [];
  const agents = (agentsQuery.data ?? []).filter((agent) => !agent.archived_at);
  const members = membersQuery.data ?? [];

  if (view === "marketplace") {
    return (
      <PluginMarketplace
        wsId={wsId}
        releases={catalogQuery.data.releases}
        installations={installations}
        canManage={canManage}
        onBack={() => setView("list")}
        onInstalled={(installationId) => setView({ detail: installationId })}
      />
    );
  }

  const detailInstallation = typeof view === "object"
    ? installations.find((installation) => installation.id === view.detail)
    : undefined;

  if (detailInstallation) {
    const releases = catalogQuery.data.releases
      .filter((release) => release.plugin_key === detailInstallation.plugin_key)
      .sort((left, right) => comparePluginVersions(right.version, left.version));
    const uploaderName = detailInstallation.uploader_id
      ? members.find((member) => member.user_id === detailInstallation.uploader_id)?.name
      : undefined;
    return (
      <PluginDetail
        wsId={wsId}
        installation={detailInstallation}
        releases={releases}
        agents={agents}
        canManage={canManage}
        uploaderName={uploaderName}
        onBack={() => setView("list")}
      />
    );
  }

  const toggleInstallation = async (installation: PluginInstallation, enabled: boolean) => {
    try {
      await applyInstallationEnabled(enabledMutation.mutateAsync, installation, wsId, enabled);
      toast.success(enabled ? t(($) => $.plugins.enabled) : t(($) => $.plugins.disabled));
    } catch (error) {
      reportError(error);
    }
  };

  return (
    <SettingsTab
      title={t(($) => $.plugins.title)}
      description={t(($) => $.plugins.description)}
      action={(
        <Button variant="outline" onClick={() => setView("marketplace")}>
          <Store />
          {t(($) => $.plugins.browse_marketplace)}
        </Button>
      )}
    >
      {catalogQuery.data.diagnostics.length > 0 ? (
        <Alert variant="destructive">
          <AlertCircle />
          <AlertTitle>{t(($) => $.plugins.catalog_degraded)}</AlertTitle>
          <AlertDescription>{t(($) => $.plugins.catalog_degraded_description)}</AlertDescription>
        </Alert>
      ) : null}

      {!canManage && !currentMember.isLoading ? (
        <Alert>
          <ShieldCheck />
          <AlertTitle>{t(($) => $.plugins.read_only)}</AlertTitle>
          <AlertDescription>{t(($) => $.plugins.read_only_description)}</AlertDescription>
        </Alert>
      ) : null}

      {installations.length === 0 ? (
        <SettingsCard>
          <div className="p-6 text-center text-body text-muted-foreground">
            {t(($) => $.plugins.empty_installed)}
          </div>
        </SettingsCard>
      ) : (
        <div>
          <SettingsCard>
            {installations.map((installation) => (
              <InstallationRow
                key={installation.id}
                installation={installation}
                canManage={canManage}
                busy={enabledMutation.isPending}
                onOpen={() => setView({ detail: installation.id })}
                onToggle={(enabled) => void toggleInstallation(installation, enabled)}
              />
            ))}
          </SettingsCard>
          <p className="mt-2 px-1 text-caption leading-5 text-muted-foreground">
            {t(($) => $.plugins.list_footnote)}
          </p>
        </div>
      )}
    </SettingsTab>
  );
}
