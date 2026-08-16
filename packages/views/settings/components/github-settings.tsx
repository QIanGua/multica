"use client";

import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { GitCommitHorizontal, Link2, PanelRight } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Label } from "@multica/ui/components/ui/label";
import { Switch } from "@multica/ui/components/ui/switch";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useAuthStore } from "@multica/core/auth";
import { useWorkspaceId } from "@multica/core/hooks";
import { useCurrentWorkspace } from "@multica/core/paths";
import { memberListOptions, workspaceKeys } from "@multica/core/workspace/queries";
import {
  deriveGitHubSettings,
  githubInstallationsOptions,
} from "@multica/core/github";
import { api } from "@multica/core/api";
import type { Workspace } from "@multica/core/types";
import { useT } from "../../i18n";
import { SettingsCard, SettingsRow } from "./settings-layout";
import { GitHubMark } from "./github-mark";

// GitHub used to be its own settings tab that linked across to Repositories,
// which linked back. They are one subject — a hosting connection is only ever a
// means of getting repositories — so both halves now mount inside the
// Repositories tab (MUL-6232), with the repository list between them: connect,
// then pick repositories, then decide what Multica writes back to GitHub.

type SettingsKey =
  | "github_enabled"
  | "github_pr_sidebar_enabled"
  | "co_authored_by_enabled"
  | "github_auto_link_prs_enabled";

/**
 * Shared writer for the workspace-level GitHub flags. Both sections below hold
 * their own `savingKey`, so a switch only ever disables itself while in flight.
 */
function useGitHubFlagWriter() {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const qc = useQueryClient();
  const [savingKey, setSavingKey] = useState<SettingsKey | null>(null);

  async function persistSetting(key: SettingsKey, next: boolean) {
    if (!workspace || savingKey) return;
    setSavingKey(key);
    try {
      const merged = {
        ...((workspace.settings as Record<string, unknown>) ?? {}),
        [key]: next,
      };
      const updated = await api.updateWorkspace(workspace.id, { settings: merged });
      qc.setQueryData(workspaceKeys.list(), (old: Workspace[] | undefined) =>
        old?.map((ws) => (ws.id === updated.id ? updated : ws)),
      );
      toast.success(t(($) => $.auto_save.toast_saved), {
        id: "settings-auto-save",
      });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.github.toast_failed));
    } finally {
      setSavingKey(null);
    }
  }

  return { savingKey, persistSetting };
}

/**
 * Read-only for every member, actionable for whoever the backend says may
 * manage the installation (`can_manage`) — the frontend never claims a right
 * the server would reject.
 */
function useGitHubInstallations() {
  const wsId = useWorkspaceId();
  const user = useAuthStore((s) => s.user);
  const { data: members = [] } = useQuery(memberListOptions(wsId));
  const canView = members.some((m) => m.user_id === user?.id);

  const { data } = useQuery({
    ...githubInstallationsOptions(wsId),
    enabled: !!wsId && canView,
  });
  const installations = data?.installations ?? [];

  return {
    installations,
    configured: data?.configured ?? false,
    canManage: data?.can_manage === true,
    connected: installations.length > 0,
    primaryInstallation: installations[0] ?? null,
  };
}

/** The GitHub App connection: master switch plus connect / disconnect. */
export function GitHubConnectionCard() {
  const { t } = useT("settings");
  const wsId = useWorkspaceId();
  const workspace = useCurrentWorkspace();
  const qc = useQueryClient();
  const { savingKey, persistSetting } = useGitHubFlagWriter();
  const { installations, configured, canManage, connected, primaryInstallation } =
    useGitHubInstallations();

  const [connecting, setConnecting] = useState(false);
  const [disconnectTarget, setDisconnectTarget] = useState<string | null>(null);
  const [disconnecting, setDisconnecting] = useState(false);

  const flags = deriveGitHubSettings(workspace);

  async function handleConnect() {
    setConnecting(true);
    try {
      const resp = await api.getGitHubConnectURL(wsId, "repositories");
      if (!resp.configured || !resp.url) {
        toast.error(t(($) => $.github.toast_not_configured));
        return;
      }
      window.open(resp.url, "_blank", "noopener");
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.github.toast_open_failed));
    } finally {
      setConnecting(false);
    }
  }

  async function handleDisconnect() {
    if (!disconnectTarget || disconnecting) return;
    setDisconnecting(true);
    try {
      await api.deleteGitHubInstallation(wsId, disconnectTarget);
      await qc.invalidateQueries({ queryKey: ["github", wsId] });
      toast.success(t(($) => $.github.toast_disconnected));
      setDisconnectTarget(null);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t(($) => $.github.toast_disconnect_failed));
    } finally {
      setDisconnecting(false);
    }
  }

  if (!workspace) return null;

  const connectedTo = t(($) => $.github.connected_to, {
    login: installations.map((i) => i.account_login).join(", "),
  });
  const status = connected
    ? primaryInstallation?.connected_by
      ? `${connectedTo} · ${t(($) => $.github.connected_by, {
          name: primaryInstallation.connected_by,
        })}`
      : connectedTo
    : canManage
      ? t(($) => $.github.connection_hint)
      : t(($) => $.github.contact_admin_to_connect);

  return (
    <>
      <SettingsCard>
        <SettingsRow
          label={
            <span className="flex items-center gap-2">
              <GitHubMark className="size-4" />
              {t(($) => $.github.connection_title)}
            </span>
          }
          description={status}
        >
          {canManage ? (
            connected && primaryInstallation ? (
              // Disconnect must stay reachable even when the master switch is
              // off — revoking the App grant is a separate intent from hiding
              // the feature.
              <Button
                variant="outline"
                size="sm"
                onClick={() => setDisconnectTarget(primaryInstallation.id)}
              >
                {t(($) => $.github.disconnect)}
              </Button>
            ) : (
              <Button
                size="sm"
                onClick={handleConnect}
                disabled={connecting || !configured}
                title={
                  !configured ? t(($) => $.github.connect_disabled_tooltip) : undefined
                }
              >
                {connecting
                  ? t(($) => $.github.connect_opening)
                  : t(($) => $.github.connect_github)}
              </Button>
            )
          ) : null}
        </SettingsRow>

        <SettingsRow
          label={t(($) => $.github.section_master)}
          description={
            flags.enabled
              ? t(($) => $.github.master_description_on)
              : t(($) => $.github.master_description_off)
          }
        >
          <Switch
            id="github-master"
            aria-label={t(($) => $.github.section_master)}
            checked={flags.enabled}
            onCheckedChange={(v) => persistSetting("github_enabled", v)}
            disabled={!canManage || savingKey === "github_enabled"}
          />
        </SettingsRow>

        {canManage && !configured ? (
          <div className="px-4 py-3 text-caption text-muted-foreground">
            {t(($) => $.github.not_configured)}{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-micro">GITHUB_APP_SLUG</code>{" "}
            {t(($) => $.github.not_configured_and)}{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-micro">
              GITHUB_WEBHOOK_SECRET
            </code>
            .
          </div>
        ) : null}

        {!canManage && connected ? (
          <div className="px-4 py-3 text-caption text-muted-foreground">
            {t(($) => $.github.read_only_hint)}
          </div>
        ) : null}
      </SettingsCard>

      <AlertDialog
        open={!!disconnectTarget}
        onOpenChange={(v) => {
          if (!v && !disconnecting) setDisconnectTarget(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t(($) => $.github.disconnect_confirm_title)}
            </AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.github.disconnect_confirm_description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={disconnecting}>
              {t(($) => $.github.disconnect_confirm_cancel)}
            </AlertDialogCancel>
            <AlertDialogAction onClick={handleDisconnect} disabled={disconnecting}>
              {disconnecting
                ? t(($) => $.github.disconnecting)
                : t(($) => $.github.disconnect_confirm_action)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}

/** What Multica writes back to GitHub, and what it shows from it. */
export function GitHubBehaviorCard() {
  const { t } = useT("settings");
  const workspace = useCurrentWorkspace();
  const { savingKey, persistSetting } = useGitHubFlagWriter();
  const { canManage } = useGitHubInstallations();

  const flags = deriveGitHubSettings(workspace);
  if (!workspace) return null;

  return (
    <SettingsCard>
      <FeatureRow
        id="github-pr-sidebar"
        icon={<PanelRight className="size-4" />}
        label={t(($) => $.github.feature_pr_sidebar_label)}
        description={t(($) => $.github.feature_pr_sidebar_description)}
        checked={flags.prSidebar}
        disabled={!canManage || !flags.enabled || savingKey === "github_pr_sidebar_enabled"}
        onCheckedChange={(v) => persistSetting("github_pr_sidebar_enabled", v)}
      />
      <FeatureRow
        id="github-coauthor"
        icon={<GitCommitHorizontal className="size-4" />}
        label={t(($) => $.github.feature_co_author_label)}
        description={
          <>
            {t(($) => $.github.feature_co_author_description_prefix)}{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-micro">
              {"Co-authored-by: multica-agent <github@multica.ai>"}
            </code>{" "}
            {t(($) => $.github.feature_co_author_description_suffix)}
          </>
        }
        checked={flags.coAuthor}
        disabled={!canManage || !flags.enabled || savingKey === "co_authored_by_enabled"}
        onCheckedChange={(v) => persistSetting("co_authored_by_enabled", v)}
      />
      <FeatureRow
        id="github-auto-link"
        icon={<Link2 className="size-4" />}
        label={t(($) => $.github.feature_auto_link_label)}
        description={t(($) => $.github.feature_auto_link_description)}
        checked={flags.autoLinkPRs}
        disabled={!canManage || !flags.enabled || savingKey === "github_auto_link_prs_enabled"}
        onCheckedChange={(v) => persistSetting("github_auto_link_prs_enabled", v)}
      />
    </SettingsCard>
  );
}

function FeatureRow({
  id,
  icon,
  label,
  description,
  checked,
  disabled,
  onCheckedChange,
}: {
  id: string;
  icon: React.ReactNode;
  label: string;
  description: React.ReactNode;
  checked: boolean;
  disabled: boolean;
  onCheckedChange: (v: boolean) => void;
}) {
  return (
    <SettingsRow
      label={
        <Label htmlFor={id} className="flex items-center gap-2 text-body font-medium">
          <span className="text-muted-foreground">{icon}</span>
          {label}
        </Label>
      }
      description={description}
    >
      <Switch
        id={id}
        checked={checked}
        disabled={disabled}
        onCheckedChange={onCheckedChange}
      />
    </SettingsRow>
  );
}
