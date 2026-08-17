"use client";

import { useMemo, useState } from "react";
import { ArrowLeft, BadgeCheck, BookOpenText, CircleCheck, Loader2, Server, ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { comparePluginVersions, useInstallPlugin, useSetPluginEnabled } from "@multica/core/plugins";
import type { PluginCatalogRelease, PluginInstallation } from "@multica/core/types";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { useT } from "../../i18n";
import { PluginLogo } from "./plugin-shared";

function SignatureBadge({ verified }: { verified: boolean }) {
  const { t } = useT("settings");
  return verified ? (
    <Badge variant="secondary" className="bg-success/10 text-success">
      <ShieldCheck />
      {t(($) => $.plugins.signed)}
    </Badge>
  ) : (
    <Badge variant="destructive">{t(($) => $.plugins.signature_unverified)}</Badge>
  );
}

/**
 * Full-content marketplace view: the latest release of every catalog
 * Plugin as an install card. Installing happens through a confirmation
 * dialog that lists what the Plugin contributes and offers to turn it on
 * for the workspace right away.
 */
export function PluginMarketplace({
  wsId,
  releases,
  installations,
  canManage,
  onBack,
  onInstalled,
}: {
  wsId: string;
  releases: PluginCatalogRelease[];
  installations: PluginInstallation[];
  canManage: boolean;
  onBack: () => void;
  onInstalled: (installationId: string) => void;
}) {
  const { t } = useT("settings");
  const installMutation = useInstallPlugin(wsId);
  const enabledMutation = useSetPluginEnabled(wsId);
  const [dialogRelease, setDialogRelease] = useState<PluginCatalogRelease | null>(null);
  const [enableAfterInstall, setEnableAfterInstall] = useState(true);
  const isPending = installMutation.isPending || enabledMutation.isPending;

  const latestReleases = useMemo(() => {
    const byKey = new Map<string, PluginCatalogRelease>();
    for (const release of releases) {
      const current = byKey.get(release.plugin_key);
      if (!current || comparePluginVersions(release.version, current.version) > 0) {
        byKey.set(release.plugin_key, release);
      }
    }
    return [...byKey.values()];
  }, [releases]);

  const installedKeys = useMemo(
    () => new Set(installations.map((installation) => installation.plugin_key)),
    [installations],
  );

  const openInstallDialog = (release: PluginCatalogRelease) => {
    setEnableAfterInstall(true);
    setDialogRelease(release);
  };

  const confirmInstall = async () => {
    if (!dialogRelease) return;
    try {
      const installed = await installMutation.mutateAsync({
        plugin_key: dialogRelease.plugin_key,
        version: dialogRelease.version,
      });
      if (enableAfterInstall) {
        await enabledMutation.mutateAsync({
          installationId: installed.id,
          enabled: true,
          binding: { scope_type: "workspace", scope_id: wsId },
        });
      }
      toast.success(t(($) => $.plugins.install_dialog.success));
      setDialogRelease(null);
      onInstalled(installed.id);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.plugins.action_failed));
    }
  };

  return (
    <div className="space-y-8">
      <header>
        <Button variant="ghost" size="sm" className="-ml-2 mb-2 text-muted-foreground" onClick={onBack}>
          <ArrowLeft />
          {t(($) => $.plugins.marketplace.back)}
        </Button>
        <h2 className="text-title-lg font-semibold tracking-tight">{t(($) => $.plugins.marketplace.title)}</h2>
        <p className="mt-1 max-w-2xl text-body leading-6 text-muted-foreground">
          {t(($) => $.plugins.marketplace.subtitle)}
        </p>
      </header>

      {latestReleases.length === 0 ? (
        <p className="text-body text-muted-foreground">{t(($) => $.plugins.marketplace.empty)}</p>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {latestReleases.map((release) => {
            const skillCount = release.contributions.filter((contribution) => contribution.type === "agent.skill.v1").length;
            const hasRemoteMCP = release.contributions.some((contribution) => contribution.type === "tool.remote-mcp.v1");
            const installed = Boolean(release.installation) || installedKeys.has(release.plugin_key);
            const installable = release.compatible === true && release.signature_verified === true;
            return (
              <div
                key={release.plugin_key}
                className="flex flex-col rounded-xl border border-surface-border bg-card p-4"
              >
                <div className="flex items-start gap-3">
                  <PluginLogo name={release.name} />
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-body font-medium">{release.name}</div>
                    <div className="mt-0.5 flex items-center gap-1 text-caption text-muted-foreground">
                      <span className="truncate">{release.publisher}</span>
                      <BadgeCheck className="size-3.5 shrink-0 text-brand" />
                      <span className="shrink-0">{t(($) => $.plugins.official)}</span>
                    </div>
                  </div>
                </div>
                <p className="mt-3 line-clamp-2 text-caption leading-5 text-muted-foreground">
                  {release.description}
                </p>
                <div className="mt-auto flex items-center justify-between gap-3 pt-4">
                  <span className="min-w-0 truncate text-caption text-muted-foreground">
                    {release.compatible !== true ? (
                      <span className="text-destructive">{t(($) => $.plugins.incompatible)}</span>
                    ) : (
                      <>
                        {skillCount > 0 ? t(($) => $.plugins.marketplace.skills_count, { count: skillCount }) : null}
                        {skillCount > 0 && hasRemoteMCP ? " · " : null}
                        {hasRemoteMCP ? t(($) => $.plugins.marketplace.mcp) : null}
                      </>
                    )}
                  </span>
                  {installed ? (
                    <span className="inline-flex shrink-0 items-center gap-1 text-caption text-muted-foreground">
                      <CircleCheck className="size-3.5 text-success" />
                      {t(($) => $.plugins.installed)}
                    </span>
                  ) : (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={!canManage || !installable || isPending}
                      onClick={() => openInstallDialog(release)}
                    >
                      {t(($) => $.plugins.install)}
                    </Button>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}

      <Dialog open={dialogRelease !== null} onOpenChange={(open) => !open && setDialogRelease(null)}>
        <DialogContent className="sm:max-w-md">
          {dialogRelease ? (
            <>
              <DialogHeader>
                <div className="flex items-start gap-3">
                  <PluginLogo name={dialogRelease.name} />
                  <div className="min-w-0">
                    <DialogTitle>
                      {t(($) => $.plugins.install_dialog.title, { name: dialogRelease.name })}
                    </DialogTitle>
                    <div className="mt-1.5 flex flex-wrap items-center gap-2 text-caption text-muted-foreground">
                      <span>
                        {t(($) => $.plugins.install_dialog.version_caption, {
                          publisher: dialogRelease.publisher,
                          version: dialogRelease.version,
                        })}
                      </span>
                      <SignatureBadge verified={dialogRelease.signature_verified === true} />
                    </div>
                  </div>
                </div>
              </DialogHeader>

              <div className="space-y-2">
                <div className="text-caption font-medium">{t(($) => $.plugins.install_dialog.capabilities)}</div>
                <ul className="space-y-2.5">
                  {dialogRelease.contributions.map((contribution) => (
                    <li key={contribution.key} className="flex items-start gap-2.5">
                      {contribution.type === "tool.remote-mcp.v1" ? (
                        <Server className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                      ) : (
                        <BookOpenText className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                      )}
                      <div className="min-w-0">
                        <div className="text-body font-medium">{contribution.name}</div>
                        {contribution.description ? (
                          <div className="text-caption leading-5 text-muted-foreground">{contribution.description}</div>
                        ) : null}
                      </div>
                    </li>
                  ))}
                </ul>
              </div>

              <label className="flex cursor-pointer items-start gap-2.5">
                <Checkbox
                  className="mt-0.5"
                  checked={enableAfterInstall}
                  disabled={isPending}
                  onCheckedChange={(checked) => setEnableAfterInstall(checked === true)}
                />
                <span className="min-w-0">
                  <span className="block text-body">{t(($) => $.plugins.install_dialog.enable_after_install)}</span>
                  <span className="mt-0.5 block text-caption leading-5 text-muted-foreground">
                    {t(($) => $.plugins.install_dialog.enable_hint)}
                  </span>
                </span>
              </label>

              <DialogFooter>
                <Button variant="ghost" disabled={isPending} onClick={() => setDialogRelease(null)}>
                  {t(($) => $.plugins.install_dialog.cancel)}
                </Button>
                <Button disabled={isPending} onClick={() => void confirmInstall()}>
                  {isPending ? <Loader2 className="animate-spin" /> : null}
                  {t(($) => $.plugins.install)}
                </Button>
              </DialogFooter>
            </>
          ) : null}
        </DialogContent>
      </Dialog>
    </div>
  );
}
