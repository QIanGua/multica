"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, CircleAlert, Loader2, PackageCheck, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { memberListOptions } from "@multica/core/workspace/queries";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow, SettingsSection, SettingsTab } from "./settings-layout";

const REVIEW_READINESS_PLUGIN_KEY = "ai.multica.software-delivery";

export function PluginsTab() {
  const { t } = useT("settings");
  const workspaceId = useWorkspaceId();
  const user = useAuthStore((state) => state.user);
  const queryClient = useQueryClient();
  const queryKey = ["workspaces", workspaceId, "plugins"] as const;
  const pluginQuery = useQuery({
    queryKey,
    queryFn: () => api.listWorkspacePlugins(workspaceId),
    enabled: !!workspaceId,
  });
  const { data: members = [] } = useQuery(memberListOptions(workspaceId));
  const member = members.find((candidate) => candidate.user_id === user?.id);
  const canManage = member?.role === "owner" || member?.role === "admin";

  const catalogEntry = pluginQuery.data?.catalog.find(
    (entry) => entry.plugin_key === REVIEW_READINESS_PLUGIN_KEY,
  );
  const installation = pluginQuery.data?.plugins.find(
    (entry) => entry.plugin_key === REVIEW_READINESS_PLUGIN_KEY,
  );

  const lifecycleMutation = useMutation({
    mutationFn: async (action: "install" | "enable" | "disable") => {
      if (action === "install") {
        return api.installWorkspacePlugin(workspaceId, REVIEW_READINESS_PLUGIN_KEY);
      }
      if (!installation) throw new Error("Plugin installation not found");
      return action === "enable"
        ? api.enableWorkspacePlugin(workspaceId, installation.id)
        : api.disableWorkspacePlugin(workspaceId, installation.id);
    },
    onSuccess: async (_, action) => {
      await queryClient.invalidateQueries({ queryKey });
      toast.success(
        action === "install"
          ? t(($) => $.plugins.toast_installed)
          : action === "enable"
            ? t(($) => $.plugins.toast_enabled)
            : t(($) => $.plugins.toast_disabled),
      );
    },
    onError: (error) => {
      toast.error(
        error instanceof Error ? error.message : t(($) => $.plugins.toast_failed),
      );
    },
  });

  const pendingAction = lifecycleMutation.variables;
  const button = !installation ? (
    <Button
      onClick={() => lifecycleMutation.mutate("install")}
      disabled={!canManage || lifecycleMutation.isPending}
    >
      {pendingAction === "install" && lifecycleMutation.isPending ? (
        <Loader2 className="animate-spin" />
      ) : null}
      {t(($) => $.plugins.install)}
    </Button>
  ) : installation.enabled ? (
    <Button
      variant="outline"
      onClick={() => lifecycleMutation.mutate("disable")}
      disabled={!canManage || lifecycleMutation.isPending}
    >
      {pendingAction === "disable" && lifecycleMutation.isPending ? (
        <Loader2 className="animate-spin" />
      ) : null}
      {t(($) => $.plugins.disable)}
    </Button>
  ) : (
    <Button
      onClick={() => lifecycleMutation.mutate("enable")}
      disabled={!canManage || lifecycleMutation.isPending}
    >
      {pendingAction === "enable" && lifecycleMutation.isPending ? (
        <Loader2 className="animate-spin" />
      ) : null}
      {t(($) => $.plugins.enable)}
    </Button>
  );

  return (
    <SettingsTab
      title={t(($) => $.page.tabs.plugins)}
      description={t(($) => $.plugins.page_description)}
    >
      <SettingsSection
        title={t(($) => $.plugins.section_title)}
        description={t(($) => $.plugins.section_description)}
      >
        <SettingsCard>
          {pluginQuery.isPending ? (
            <div className="flex min-h-40 items-center justify-center text-muted-foreground">
              <Loader2 className="size-5 animate-spin" aria-label={t(($) => $.plugins.loading)} />
            </div>
          ) : pluginQuery.isError || !catalogEntry ? (
            <div className="flex min-h-40 flex-col items-center justify-center gap-3 px-6 text-center">
              <CircleAlert className="size-5 text-destructive" />
              <p className="text-body text-muted-foreground">
                {t(($) => $.plugins.load_failed)}
              </p>
              <Button variant="outline" onClick={() => pluginQuery.refetch()}>
                {t(($) => $.plugins.retry)}
              </Button>
            </div>
          ) : (
            <>
              <SettingsRow
                align="start"
                label={
                  <span className="flex flex-wrap items-center gap-2">
                    <span>{t(($) => $.plugins.software_delivery_name)}</span>
                    <Badge variant="secondary">{t(($) => $.plugins.demo_badge)}</Badge>
                    <Badge variant="outline" className="font-normal">
                      {installation?.active_version ?? catalogEntry.version}
                    </Badge>
                  </span>
                }
                description={t(($) => $.plugins.software_delivery_description)}
              >
                <div className="flex flex-col items-stretch gap-2 sm:items-end">
                  {button}
                  {!canManage ? (
                    <span className="text-caption text-muted-foreground">
                      {t(($) => $.plugins.admin_only)}
                    </span>
                  ) : null}
                </div>
              </SettingsRow>
              <SettingsRow
                label={t(($) => $.plugins.status_label)}
                description={
                  installation?.enabled
                    ? t(($) => $.plugins.status_active_description)
                    : installation
                      ? t(($) => $.plugins.status_installed_description)
                      : t(($) => $.plugins.status_available_description)
                }
              >
                <Badge
                  variant={installation?.enabled ? "secondary" : "outline"}
                  className="gap-1.5 font-normal"
                >
                  {installation?.enabled ? <Check className="size-3" /> : null}
                  {installation?.enabled
                    ? t(($) => $.plugins.status_active)
                    : installation
                      ? t(($) => $.plugins.status_installed)
                      : t(($) => $.plugins.status_available)}
                </Badge>
              </SettingsRow>
              <SettingsRow
                align="start"
                label={t(($) => $.plugins.contribution_label)}
                description={t(($) => $.plugins.contribution_description)}
              >
                <div className="flex flex-wrap justify-start gap-2 sm:justify-end">
                  {(installation?.contributions ?? catalogEntry.contributions).map((contribution) => (
                    <Badge key={contribution} variant="outline" className="font-mono font-normal">
                      {contribution}
                    </Badge>
                  ))}
                </div>
              </SettingsRow>
              <SettingsRow
                align="start"
                label={
                  <span className="flex items-center gap-2">
                    <ShieldCheck className="size-4 text-success" />
                    {t(($) => $.plugins.execution_label)}
                  </span>
                }
                description={t(($) => $.plugins.execution_description)}
              >
                <span className="inline-flex items-center gap-1.5 text-caption text-muted-foreground">
                  <PackageCheck className="size-4" />
                  {t(($) => $.plugins.official_bundle)}
                </span>
              </SettingsRow>
            </>
          )}
        </SettingsCard>
      </SettingsSection>
    </SettingsTab>
  );
}
