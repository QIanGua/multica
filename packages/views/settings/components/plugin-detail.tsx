"use client";

import { useEffect, useRef, useState } from "react";
import { ArrowLeft, Check, Loader2, Plus, Server, ShieldCheck, X } from "lucide-react";
import { toast } from "sonner";
import {
  comparePluginVersions,
  useApprovePluginRemoteMCPTools,
  useConfigurePluginRemoteMCP,
  useRevokePluginRemoteMCPCredential,
  useRollbackPlugin,
  useSetPluginEnabled,
  useStartPluginRemoteMCPOAuth,
  useTestPluginRemoteMCP,
  useUninstallPlugin,
  useUpgradePlugin,
} from "@multica/core/plugins";
import type {
  Agent,
  PluginCatalogRelease,
  PluginInstallation,
  PluginRemoteMCPConfig,
  RemoteMCPDiscoveryResponse,
} from "@multica/core/types";
import { Badge } from "@multica/ui/components/ui/badge";
import { Button } from "@multica/ui/components/ui/button";
import { RadioGroup, RadioGroupItem } from "@multica/ui/components/ui/radio-group";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { Switch } from "@multica/ui/components/ui/switch";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { isDesktopShell } from "../../platform/local-directory";
import { openExternal } from "../../platform/open-external";
import { PluginRemoteMCPAdvanced } from "./plugin-remote-mcp-advanced";
import {
  applyInstallationEnabled,
  InstallationHealth,
  installationNeedsSetup,
  isRemoteMCPConnected,
  NeedsSetupBadge,
  pendingDiscovery,
  PluginLogo,
} from "./plugin-shared";
import { PluginToolReviewDialog } from "./plugin-tool-review-dialog";
import { SettingsCard, SettingsSection } from "./settings-layout";

type ReviewTarget = {
  contributionKey: string;
  discovery: RemoteMCPDiscoveryResponse;
};

function StepMarker({ index, done }: { index: number; done: boolean }) {
  return (
    <span
      aria-hidden
      className={cn(
        "flex size-5 shrink-0 items-center justify-center rounded-full border text-caption font-medium",
        done
          ? "border-transparent bg-success/15 text-success"
          : "border-surface-border bg-background text-muted-foreground",
      )}
    >
      {done ? <Check className="size-3" /> : index}
    </span>
  );
}

/**
 * Per-Plugin settings page: setup steps until the remote MCP contributions
 * are ready, then connection management, agent availability, and version /
 * publisher metadata. `installation` is re-derived from the installations
 * query by the parent on every render — never copied into state here.
 */
export function PluginDetail({
  wsId,
  installation,
  releases,
  agents,
  canManage,
  uploaderName,
  onBack,
}: {
  wsId: string;
  installation: PluginInstallation;
  /** Catalog releases for this plugin key, sorted newest first. */
  releases: PluginCatalogRelease[];
  agents: Agent[];
  canManage: boolean;
  uploaderName?: string;
  onBack: () => void;
}) {
  const { t } = useT("settings");
  const enabledMutation = useSetPluginEnabled(wsId);
  const upgradeMutation = useUpgradePlugin(wsId);
  const rollbackMutation = useRollbackPlugin(wsId);
  const uninstallMutation = useUninstallPlugin(wsId);
  const configureMutation = useConfigurePluginRemoteMCP(wsId);
  const oauthMutation = useStartPluginRemoteMCPOAuth(wsId);
  const testMutation = useTestPluginRemoteMCP(wsId);
  const approveMutation = useApprovePluginRemoteMCPTools(wsId);
  const revokeMutation = useRevokePluginRemoteMCPCredential(wsId);

  const [reviewTarget, setReviewTarget] = useState<ReviewTarget | null>(null);
  const [advancedOpen, setAdvancedOpen] = useState<Record<string, boolean>>({});
  const [scopeOverride, setScopeOverride] = useState<"all" | "selected" | null>(null);
  // Contribution keys whose pending discovery already auto-opened the review
  // dialog once — closing the dialog must not immediately re-open it.
  const autoOpenedRef = useRef<Set<string>>(new Set());

  const isPrivate = installation.source_kind === "private_dev";
  const needsSetup = installationNeedsSetup(installation);
  const busy = enabledMutation.isPending || upgradeMutation.isPending
    || rollbackMutation.isPending || uninstallMutation.isPending;
  const connectionBusy = configureMutation.isPending || oauthMutation.isPending
    || testMutation.isPending || approveMutation.isPending || revokeMutation.isPending;

  const reportError = (error: unknown) => {
    toast.error(error instanceof Error ? error.message : t(($) => $.plugins.action_failed));
  };

  const openReviewWith = (contributionKey: string, discovery: RemoteMCPDiscoveryResponse) => {
    autoOpenedRef.current.add(contributionKey);
    setReviewTarget({ contributionKey, discovery });
  };

  // Auto-open the review dialog when a pending discovery appears — right
  // after an OAuth return or a configure/test that produced new tools.
  useEffect(() => {
    if (reviewTarget) return;
    for (const config of installation.remote_mcp) {
      const discovery = pendingDiscovery(config);
      if (discovery && !autoOpenedRef.current.has(config.contribution_key)) {
        autoOpenedRef.current.add(config.contribution_key);
        setReviewTarget({ contributionKey: config.contribution_key, discovery });
        return;
      }
    }
  }, [installation.remote_mcp, reviewTarget]);

  const toggleEnabled = async (enabled: boolean) => {
    try {
      await applyInstallationEnabled(enabledMutation.mutateAsync, installation, wsId, enabled);
      toast.success(enabled ? t(($) => $.plugins.enabled) : t(($) => $.plugins.disabled));
    } catch (error) {
      reportError(error);
    }
  };

  /** One-click connect for contributions shipping a default endpoint. */
  const connectQuick = async (config: PluginRemoteMCPConfig) => {
    const endpoint = (config.endpoint ?? "").trim() || config.default_endpoint || "";
    const failurePolicy = config.failure_policy === "optional" ? "optional" : "required";
    try {
      if (config.preferred_auth === "oauth") {
        const result = await oauthMutation.mutateAsync({
          installationId: installation.id,
          contributionKey: config.contribution_key,
          request: {
            endpoint,
            public_config: config.public_config ?? {},
            failure_policy: failurePolicy,
            return_to: `${window.location.pathname}${window.location.search}`,
          },
        });
        if (!result.authorization_url) {
          throw new Error(t(($) => $.plugins.remote_mcp.oauth_connect_failed));
        }
        if (isDesktopShell()) {
          openExternal(result.authorization_url);
        } else {
          window.location.assign(result.authorization_url);
        }
        return;
      }
      const result = await configureMutation.mutateAsync({
        installationId: installation.id,
        contributionKey: config.contribution_key,
        request: {
          endpoint,
          public_config: config.public_config ?? {},
          auth_type: "none",
          failure_policy: failurePolicy,
        },
      });
      toast.success(t(($) => $.plugins.remote_mcp.configured_success));
      openReviewWith(config.contribution_key, result);
    } catch (error) {
      reportError(error);
    }
  };

  const runTest = async (config: PluginRemoteMCPConfig, { forceReview }: { forceReview: boolean }) => {
    try {
      const result = await testMutation.mutateAsync({
        installationId: installation.id,
        contributionKey: config.contribution_key,
      });
      const digestDiffers = result.discovered_schema_digest !== (config.schema_digest ?? "");
      const shouldReview = result.discovered_tools.length > 0
        && (forceReview || config.reviewed !== true || digestDiffers);
      if (shouldReview) {
        openReviewWith(config.contribution_key, result);
      } else {
        toast.success(t(($) => $.plugins.remote_mcp.test_success));
      }
    } catch (error) {
      reportError(error);
    }
  };

  const approveTools = async (tools: string[]) => {
    if (!reviewTarget) return;
    try {
      await approveMutation.mutateAsync({
        installationId: installation.id,
        contributionKey: reviewTarget.contributionKey,
        tools,
      });
      setReviewTarget(null);
      toast.success(t(($) => $.plugins.remote_mcp.approved_success));
    } catch (error) {
      reportError(error);
    }
  };

  const revokeCredential = (config: PluginRemoteMCPConfig) => {
    revokeMutation.mutateAsync({ installationId: installation.id, contributionKey: config.contribution_key })
      .then(() => toast.success(t(($) => $.plugins.remote_mcp.revoked_success)))
      .catch(reportError);
  };

  // ----- Scope -----
  const workspaceBinding = installation.bindings.find(
    (binding) => binding.scope_type === "workspace" && binding.enabled === true,
  );
  const enabledAgentBindings = installation.bindings.filter(
    (binding) => binding.scope_type === "agent" && binding.enabled === true,
  );
  const derivedScope: "all" | "selected" = workspaceBinding
    ? "all"
    : enabledAgentBindings.length > 0 ? "selected" : "all";
  const scopeMode = scopeOverride ?? derivedScope;
  const availableAgents = agents.filter(
    (agent) => !enabledAgentBindings.some((binding) => binding.scope_id === agent.id),
  );

  const handleScopeChange = async (value: "all" | "selected") => {
    if (value === scopeMode) return;
    if (value === "selected") {
      // No mutation yet — the switch happens when the first agent is added,
      // so the installation never flaps to disabled in between.
      setScopeOverride("selected");
      return;
    }
    setScopeOverride(null);
    if (derivedScope === "all") return;
    try {
      await enabledMutation.mutateAsync({
        installationId: installation.id,
        enabled: true,
        binding: { scope_type: "workspace", scope_id: wsId },
      });
      for (const binding of enabledAgentBindings) {
        await enabledMutation.mutateAsync({
          installationId: installation.id,
          enabled: false,
          binding: { scope_type: "agent", scope_id: binding.scope_id },
        });
      }
    } catch (error) {
      reportError(error);
    }
  };

  const addAgent = async (agentId: string) => {
    try {
      // Enable the agent binding first so the installation never flaps to
      // disabled while the workspace binding is being turned off.
      await enabledMutation.mutateAsync({
        installationId: installation.id,
        enabled: true,
        binding: { scope_type: "agent", scope_id: agentId },
      });
      if (workspaceBinding) {
        await enabledMutation.mutateAsync({
          installationId: installation.id,
          enabled: false,
          binding: { scope_type: "workspace", scope_id: wsId },
        });
      }
      setScopeOverride(null);
    } catch (error) {
      reportError(error);
    }
  };

  const removeAgent = (agentId: string) => {
    enabledMutation.mutateAsync({
      installationId: installation.id,
      enabled: false,
      binding: { scope_type: "agent", scope_id: agentId },
    }).catch(reportError);
  };

  // ----- About -----
  const latestRelease = releases[0];
  const upgradeRelease = latestRelease
    && comparePluginVersions(latestRelease.version, installation.desired_version) > 0
    && latestRelease.compatible === true
    ? latestRelease
    : null;
  const isLatest = !latestRelease
    || comparePluginVersions(latestRelease.version, installation.desired_version) <= 0;
  const rollbackVersion = releases
    .find((release) => comparePluginVersions(release.version, installation.desired_version) < 0)?.version
    ?? [...installation.available_versions]
      .sort((left, right) => comparePluginVersions(right, left))
      .find((version) => comparePluginVersions(version, installation.desired_version) < 0);
  const currentRelease = releases.find((release) => release.version === installation.desired_version);
  const signingKey = currentRelease?.signature_key_id
    || (installation.manifest_digest || "").replace(/^sha256:/, "").slice(0, 12);

  const reviewConfig = reviewTarget
    ? installation.remote_mcp.find((config) => config.contribution_key === reviewTarget.contributionKey)
    : undefined;
  const unreadyConfigs = installation.remote_mcp.filter((config) => config.ready !== true);
  const hasRemoteMCP = installation.remote_mcp.length > 0;

  const credentialStateLabel = (config: PluginRemoteMCPConfig) =>
    config.credential_state === "configured"
      ? t(($) => $.plugins.connection.encrypted_note)
      : config.credential_state === "revoked"
        ? t(($) => $.plugins.remote_mcp.credential_states.revoked)
        : config.credential_state === "not_required"
          ? t(($) => $.plugins.remote_mcp.credential_states.not_required)
          : t(($) => $.plugins.remote_mcp.credential_states.missing);

  const authTypeLabel = (config: PluginRemoteMCPConfig) => {
    const value = config.auth_type;
    return value === "oauth" || value === "bearer" || value === "header" || value === "none"
      ? t(($) => $.plugins.remote_mcp.auth_types[value])
      : t(($) => $.plugins.remote_mcp.auth_types.none);
  };

  return (
    <div className="space-y-8">
      <header>
        <Button variant="ghost" size="sm" className="-ml-2 mb-2 text-muted-foreground" onClick={onBack}>
          <ArrowLeft />
          {t(($) => $.plugins.title)}
        </Button>
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
          <div className="flex min-w-0 items-start gap-3">
            <PluginLogo name={installation.display_name} className="size-10 rounded-xl text-title-sm" />
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <h2 className="text-title-lg font-semibold tracking-tight">{installation.display_name}</h2>
                <Badge variant="outline">
                  {isPrivate ? t(($) => $.plugins.private) : t(($) => $.plugins.official)}
                </Badge>
                {installation.signature_verified === true ? (
                  <Badge variant="secondary" className="bg-success/10 text-success">
                    <ShieldCheck />
                    {t(($) => $.plugins.signed)}
                  </Badge>
                ) : (
                  <Badge variant="destructive">
                    {isPrivate ? t(($) => $.plugins.unverified) : t(($) => $.plugins.signature_unverified)}
                  </Badge>
                )}
              </div>
              <p className="mt-1 break-all text-caption leading-5 text-muted-foreground">
                {installation.description ? <>{installation.description}{" · "}</> : null}
                <span className="font-mono">{installation.plugin_key}</span>
              </p>
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-3">
            {needsSetup ? <NeedsSetupBadge /> : <InstallationHealth installation={installation} />}
            <Switch
              checked={installation.enabled === true}
              disabled={!canManage || busy}
              aria-label={installation.display_name}
              onCheckedChange={(checked) => void toggleEnabled(checked)}
            />
          </div>
        </div>
      </header>

      {hasRemoteMCP && needsSetup ? (
        <div className="rounded-xl border border-warning/40 bg-warning/5 p-4 sm:p-5">
          <div className="text-body font-medium">
            {t(($) => $.plugins.setup.title, { name: installation.display_name })}
          </div>
          <p className="mt-1 text-caption leading-5 text-muted-foreground">
            {t(($) => $.plugins.setup.caption)}
          </p>
          <div className="mt-4 space-y-5">
            {unreadyConfigs.map((config) => {
              const connected = isRemoteMCPConnected(config);
              const discovery = pendingDiscovery(config);
              const canQuickConnect = Boolean(config.default_endpoint)
                && (config.preferred_auth === "oauth" || config.preferred_auth === "none");
              return (
                <div key={config.contribution_key} className="space-y-3">
                  {unreadyConfigs.length > 1 ? (
                    <div className="font-mono text-caption text-muted-foreground">{config.contribution_key}</div>
                  ) : null}
                  <div className="flex items-center gap-3">
                    <StepMarker index={1} done={connected} />
                    <div className="min-w-0 flex-1">
                      <div className="text-body font-medium">{t(($) => $.plugins.setup.step_connect)}</div>
                      {config.default_endpoint ? (
                        <div className="truncate text-caption text-muted-foreground">{config.default_endpoint}</div>
                      ) : null}
                    </div>
                    {connected ? null : canQuickConnect ? (
                      <Button
                        size="sm"
                        disabled={!canManage || connectionBusy}
                        onClick={() => void connectQuick(config)}
                      >
                        {oauthMutation.isPending || configureMutation.isPending ? <Loader2 className="animate-spin" /> : null}
                        {t(($) => $.plugins.remote_mcp.connect)}
                      </Button>
                    ) : (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={!canManage}
                        onClick={() => setAdvancedOpen((current) => ({ ...current, [config.contribution_key]: true }))}
                      >
                        {t(($) => $.plugins.setup.configure)}
                      </Button>
                    )}
                  </div>
                  <div className={cn("flex items-center gap-3", !discovery && "opacity-50")}>
                    <StepMarker index={2} done={false} />
                    <div className="min-w-0 flex-1 text-body font-medium">
                      {t(($) => $.plugins.setup.step_review)}
                    </div>
                    {discovery ? (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={!canManage || connectionBusy}
                        onClick={() => openReviewWith(config.contribution_key, discovery)}
                      >
                        {t(($) => $.plugins.setup.review_tools)}
                      </Button>
                    ) : null}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      ) : null}

      {hasRemoteMCP ? (
        <SettingsSection title={t(($) => $.plugins.connection.title)}>
          {installation.remote_mcp.map((config) => {
            const configured = (config.config_revision ?? 0) > 0;
            const connected = isRemoteMCPConnected(config);
            const readCount = config.approved_tools.filter((tool) => tool.risk !== "write").length;
            const writeCount = config.approved_tools.length - readCount;
            return (
              <SettingsCard key={config.contribution_key}>
                {configured ? (
                  <div className="flex flex-wrap items-center gap-3 px-4 py-3.5">
                    <Server className="size-4 shrink-0 text-muted-foreground" />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="break-all font-mono text-body">
                          {config.endpoint_domain || config.endpoint}
                        </span>
                        {connected ? (
                          <Badge variant="secondary" className="bg-success/10 text-success">
                            {t(($) => $.plugins.remote_mcp.connected)}
                          </Badge>
                        ) : (
                          <Badge variant="destructive">{credentialStateLabel(config)}</Badge>
                        )}
                      </div>
                      <div className="mt-0.5 text-caption text-muted-foreground">
                        {authTypeLabel(config)}
                        {config.credential_state === "configured" ? <>{" · "}{t(($) => $.plugins.connection.encrypted_note)}</> : null}
                        {installation.remote_mcp.length > 1 ? <>{" · "}<span className="font-mono">{config.contribution_key}</span></> : null}
                      </div>
                    </div>
                    <div className="flex shrink-0 items-center gap-1">
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={!canManage || connectionBusy}
                        onClick={() => void runTest(config, { forceReview: false })}
                      >
                        {testMutation.isPending ? <Loader2 className="animate-spin" /> : null}
                        {t(($) => $.plugins.remote_mcp.test)}
                      </Button>
                      {config.credential_state === "configured" ? (
                        <Button
                          size="sm"
                          variant="ghost"
                          disabled={!canManage || connectionBusy}
                          onClick={() => revokeCredential(config)}
                        >
                          {t(($) => $.plugins.remote_mcp.revoke)}
                        </Button>
                      ) : null}
                    </div>
                  </div>
                ) : null}
                {configured && config.approved_tools.length > 0 ? (
                  <div className="px-4 py-3.5">
                    <div className="flex flex-wrap items-center justify-between gap-3">
                      <div>
                        <div className="text-body font-medium">
                          {t(($) => $.plugins.connection.approved_tools_count, { count: config.approved_tools.length })}
                        </div>
                        <div className="mt-0.5 text-caption text-muted-foreground">
                          {t(($) => $.plugins.connection.tool_counts, { read: readCount, write: writeCount })}
                        </div>
                      </div>
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={!canManage || connectionBusy}
                        onClick={() => void runTest(config, { forceReview: true })}
                      >
                        {t(($) => $.plugins.connection.re_review)}
                      </Button>
                    </div>
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {config.approved_tools.map((tool) => (
                        <span
                          key={tool.name}
                          className="inline-flex items-center gap-1.5 rounded-md bg-muted px-2 py-0.5 font-mono text-caption"
                        >
                          {tool.name}
                          {tool.risk === "write" ? (
                            <span aria-hidden className="size-1.5 rounded-full bg-warning" />
                          ) : null}
                        </span>
                      ))}
                    </div>
                    <p className="mt-2 text-caption leading-5 text-muted-foreground">
                      {t(($) => $.plugins.connection.schema_pinned_note)}
                    </p>
                  </div>
                ) : null}
                <div className="px-4 py-3.5">
                  <PluginRemoteMCPAdvanced
                    key={`${installation.id}:${config.contribution_key}:${config.config_revision ?? 0}`}
                    wsId={wsId}
                    installationId={installation.id}
                    config={config}
                    canManage={canManage}
                    open={advancedOpen[config.contribution_key] === true}
                    onOpenChange={(open) => setAdvancedOpen((current) => ({ ...current, [config.contribution_key]: open }))}
                    onDiscovery={(result) => openReviewWith(config.contribution_key, result)}
                  />
                </div>
              </SettingsCard>
            );
          })}
        </SettingsSection>
      ) : null}

      <SettingsSection title={t(($) => $.plugins.scope.title)}>
        <SettingsCard>
          <div className="px-4 py-3.5">
            <RadioGroup
              className="gap-3"
              value={scopeMode}
              onValueChange={(value) => {
                if (value === "all" || value === "selected") void handleScopeChange(value);
              }}
            >
              <label className="flex cursor-pointer items-start gap-3">
                <RadioGroupItem className="mt-0.5" value="all" disabled={!canManage || busy} />
                <span className="min-w-0">
                  <span className="block text-body font-medium">{t(($) => $.plugins.scope.all_agents)}</span>
                  <span className="mt-0.5 block text-caption text-muted-foreground">
                    {t(($) => $.plugins.scope.all_agents_hint)}
                  </span>
                </span>
              </label>
              <label className="flex cursor-pointer items-start gap-3">
                <RadioGroupItem className="mt-0.5" value="selected" disabled={!canManage || busy} />
                <span className="block text-body font-medium">{t(($) => $.plugins.scope.selected_agents)}</span>
              </label>
            </RadioGroup>
            {scopeMode === "selected" ? (
              <div className="mt-3 flex flex-wrap items-center gap-2 pl-7">
                {enabledAgentBindings.map((binding) => {
                  const agent = agents.find((candidate) => candidate.id === binding.scope_id);
                  const name = agent?.name ?? t(($) => $.plugins.unknown_agent);
                  return (
                    <span
                      key={binding.scope_id}
                      className="inline-flex items-center gap-1.5 rounded-full border border-surface-border bg-muted/40 py-0.5 pl-1 pr-1.5 text-caption"
                    >
                      <span
                        aria-hidden
                        className="flex size-4 items-center justify-center rounded-full bg-muted text-micro font-medium text-muted-foreground"
                      >
                        {(name.trim().charAt(0) || "#").toUpperCase()}
                      </span>
                      {name}
                      <button
                        type="button"
                        aria-label={t(($) => $.plugins.scope.remove_agent, { name })}
                        disabled={!canManage || busy}
                        className="text-muted-foreground transition-colors hover:text-foreground disabled:pointer-events-none disabled:opacity-50"
                        onClick={() => removeAgent(binding.scope_id)}
                      >
                        <X className="size-3" />
                      </button>
                    </span>
                  );
                })}
                {availableAgents.length > 0 ? (
                  <Select
                    items={availableAgents.map((agent) => ({ value: agent.id, label: agent.name }))}
                    value={null}
                    onValueChange={(value) => {
                      if (value) void addAgent(value);
                    }}
                    disabled={!canManage || busy}
                  >
                    <SelectTrigger size="sm" aria-label={t(($) => $.plugins.scope.add_agent)}>
                      <Plus className="size-3.5 text-muted-foreground" />
                      <SelectValue placeholder={t(($) => $.plugins.scope.add_agent)} />
                    </SelectTrigger>
                    <SelectContent>
                      {availableAgents.map((agent) => (
                        <SelectItem key={agent.id} value={agent.id}>{agent.name}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : null}
              </div>
            ) : null}
          </div>
        </SettingsCard>
      </SettingsSection>

      <SettingsSection title={t(($) => $.plugins.about.title)}>
        <SettingsCard>
          <div className="flex min-h-16 flex-col gap-3 px-4 py-3.5 sm:flex-row sm:items-center sm:justify-between sm:gap-8">
            <div className="min-w-0 flex-1">
              <div className="text-body font-medium">{t(($) => $.plugins.about.version)}</div>
              <div className="mt-0.5 text-caption leading-5 text-muted-foreground">
                {isLatest
                  ? t(($) => $.plugins.about.version_latest, { version: installation.desired_version })
                  : t(($) => $.plugins.about.version_current, { version: installation.desired_version })}
              </div>
            </div>
            <div className="flex shrink-0 items-center gap-2">
              {upgradeRelease ? (
                <Button
                  size="sm"
                  variant="outline"
                  disabled={!canManage || busy}
                  onClick={() => upgradeMutation.mutateAsync({
                    installationId: installation.id,
                    plugin_key: installation.plugin_key,
                    version: upgradeRelease.version,
                  }).then(() => toast.success(t(($) => $.plugins.upgraded))).catch(reportError)}
                >
                  {upgradeMutation.isPending ? <Loader2 className="animate-spin" /> : null}
                  {t(($) => $.plugins.upgrade_to, { version: upgradeRelease.version })}
                </Button>
              ) : null}
              {rollbackVersion ? (
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={!canManage || busy}
                  onClick={() => rollbackMutation.mutateAsync({ installationId: installation.id, version: rollbackVersion })
                    .then(() => toast.success(t(($) => $.plugins.rolled_back)))
                    .catch(reportError)}
                >
                  {rollbackMutation.isPending ? <Loader2 className="animate-spin" /> : null}
                  {t(($) => $.plugins.rollback_to, { version: rollbackVersion })}
                </Button>
              ) : null}
            </div>
          </div>
          <div className="flex min-h-16 flex-col gap-3 px-4 py-3.5 sm:flex-row sm:items-center sm:justify-between sm:gap-8">
            <div className="min-w-0 flex-1">
              <div className="text-body font-medium">{t(($) => $.plugins.about.publisher)}</div>
              <div className="mt-0.5 break-all text-caption leading-5 text-muted-foreground">
                {isPrivate ? (
                  <>
                    {installation.publisher}{" · "}{t(($) => $.plugins.private_upload)}
                    {installation.uploader_id ? (
                      <>{" · "}{t(($) => $.plugins.uploaded_by)}{" "}{uploaderName ?? t(($) => $.plugins.unknown_member)}</>
                    ) : null}
                  </>
                ) : (
                  t(($) => $.plugins.about.publisher_meta, {
                    publisher: installation.publisher,
                    key: signingKey,
                  })
                )}
              </div>
            </div>
            <Button
              size="sm"
              variant="ghost"
              className="shrink-0 text-destructive hover:bg-destructive/10 hover:text-destructive"
              disabled={!canManage || busy}
              onClick={() => uninstallMutation.mutateAsync(installation.id)
                .then(() => {
                  toast.success(t(($) => $.plugins.uninstalled));
                  onBack();
                })
                .catch(reportError)}
            >
              {uninstallMutation.isPending ? <Loader2 className="animate-spin" /> : null}
              {t(($) => $.plugins.uninstall)}
            </Button>
          </div>
        </SettingsCard>
      </SettingsSection>

      {reviewTarget ? (
        <PluginToolReviewDialog
          open
          onOpenChange={(open) => {
            if (!open) setReviewTarget(null);
          }}
          pluginName={installation.display_name}
          endpointDomain={reviewConfig?.endpoint_domain}
          discovery={reviewTarget.discovery}
          onApprove={(tools) => void approveTools(tools)}
          approving={approveMutation.isPending}
        />
      ) : null}
    </div>
  );
}
