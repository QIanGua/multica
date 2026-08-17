"use client";

import { useMemo, useState } from "react";
import { Loader2 } from "lucide-react";
import { toast } from "sonner";
import {
  useConfigurePluginRemoteMCP,
  useStartPluginRemoteMCPOAuth,
} from "@multica/core/plugins";
import type { PluginRemoteMCPConfig, RemoteMCPDiscoveryResponse } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import { Input } from "@multica/ui/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import { Textarea } from "@multica/ui/components/ui/textarea";
import { useT } from "../../i18n";
import { isDesktopShell } from "../../platform/local-directory";
import { openExternal } from "../../platform/open-external";

type RemoteMCPAuthType = "none" | "oauth" | "bearer" | "header";

const ALL_AUTH_TYPES: RemoteMCPAuthType[] = ["none", "oauth", "bearer", "header"];

function isAuthType(value: string | undefined): value is RemoteMCPAuthType {
  return value === "none" || value === "oauth" || value === "bearer" || value === "header";
}

/**
 * Manual endpoint/auth configuration for a remote MCP contribution.
 * Collapsed by default; the setup card expands it for contributions that
 * cannot be connected with one click. On a successful configure the fresh
 * discovery result is handed to the parent, which opens the review dialog.
 */
export function PluginRemoteMCPAdvanced({
  wsId,
  installationId,
  config,
  canManage,
  open,
  onOpenChange,
  onDiscovery,
}: {
  wsId: string;
  installationId: string;
  config: PluginRemoteMCPConfig;
  canManage: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onDiscovery: (result: RemoteMCPDiscoveryResponse) => void;
}) {
  const { t } = useT("settings");
  const configureMutation = useConfigurePluginRemoteMCP(wsId);
  const oauthMutation = useStartPluginRemoteMCPOAuth(wsId);
  const authModes = useMemo(() => {
    const supported = (config.supported_auth ?? []).filter(isAuthType);
    return supported.length > 0 ? supported : ALL_AUTH_TYPES;
  }, [config.supported_auth]);
  const [endpoint, setEndpoint] = useState(config.endpoint ?? config.default_endpoint ?? "");
  const [authType, setAuthType] = useState<RemoteMCPAuthType>(() => {
    if (isAuthType(config.auth_type) && authModes.includes(config.auth_type)) return config.auth_type;
    if (isAuthType(config.preferred_auth) && authModes.includes(config.preferred_auth)) return config.preferred_auth;
    return authModes[0] ?? "none";
  });
  const [authHeader, setAuthHeader] = useState(config.auth_header ?? "Authorization");
  const [credential, setCredential] = useState("");
  const [oauthScope, setOAuthScope] = useState("");
  const [oauthClientId, setOAuthClientId] = useState("");
  const [oauthClientSecret, setOAuthClientSecret] = useState("");
  const [oauthAuthorizationEndpoint, setOAuthAuthorizationEndpoint] = useState("");
  const [oauthTokenEndpoint, setOAuthTokenEndpoint] = useState("");
  const [oauthTokenAuthMethod, setOAuthTokenAuthMethod] = useState<"none" | "client_secret_basic" | "client_secret_post">("none");
  const [publicConfig, setPublicConfig] = useState(() => JSON.stringify(config.public_config ?? {}, null, 2));
  const [failurePolicy, setFailurePolicy] = useState<"required" | "optional">(
    config.failure_policy === "optional" ? "optional" : "required",
  );
  const isPending = configureMutation.isPending || oauthMutation.isPending;

  const configure = async () => {
    let parsedPublicConfig: Record<string, unknown>;
    try {
      const parsed: unknown = JSON.parse(publicConfig);
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error();
      parsedPublicConfig = parsed as Record<string, unknown>;
    } catch {
      toast.error(t(($) => $.plugins.remote_mcp.invalid_public_config));
      return;
    }
    try {
      if (authType === "oauth") {
        const result = await oauthMutation.mutateAsync({
          installationId,
          contributionKey: config.contribution_key,
          request: {
            endpoint: endpoint.trim() || config.default_endpoint,
            public_config: parsedPublicConfig,
            failure_policy: failurePolicy,
            scope: oauthScope.trim() || undefined,
            client_id: oauthClientId.trim() || undefined,
            client_secret: oauthClientSecret || undefined,
            token_endpoint_auth_method: oauthClientId.trim() ? oauthTokenAuthMethod : undefined,
            authorization_endpoint: oauthAuthorizationEndpoint.trim() || undefined,
            token_endpoint: oauthTokenEndpoint.trim() || undefined,
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
        installationId,
        contributionKey: config.contribution_key,
        request: {
          endpoint: endpoint.trim(),
          public_config: parsedPublicConfig,
          auth_type: authType,
          auth_header: authType === "header" ? authHeader.trim() : undefined,
          credential: authType === "none" ? undefined : credential,
          failure_policy: failurePolicy,
        },
      });
      setCredential("");
      toast.success(t(($) => $.plugins.remote_mcp.configured_success));
      onDiscovery(result);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : t(($) => $.plugins.action_failed));
    }
  };

  return (
    <details
      className="group rounded-lg border border-surface-border bg-background"
      open={open}
      onToggle={(event) => onOpenChange(event.currentTarget.open)}
    >
      <summary className="cursor-pointer select-none px-3 py-2 text-caption font-medium">
        {t(($) => $.plugins.remote_mcp.advanced)}
      </summary>
      <div className="grid gap-3 border-t border-surface-border p-3 sm:grid-cols-2">
        <label className="space-y-1 text-caption sm:col-span-2">
          <span className="font-medium">{t(($) => $.plugins.remote_mcp.endpoint)}</span>
          <Input
            value={endpoint}
            onChange={(event) => setEndpoint(event.target.value)}
            placeholder={t(($) => $.plugins.remote_mcp.endpoint_placeholder)}
            disabled={!canManage || isPending}
          />
        </label>
        {authModes.length > 1 ? (
          <label className="space-y-1 text-caption">
            <span className="font-medium">{t(($) => $.plugins.remote_mcp.auth)}</span>
            <Select
              items={authModes.map((value) => ({
                value,
                label: t(($) => $.plugins.remote_mcp.auth_types[value]),
              }))}
              value={authType}
              onValueChange={(value) => value && setAuthType(value)}
              disabled={!canManage || isPending}
            >
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>
                {authModes.map((value) => (
                  <SelectItem key={value} value={value}>
                    {t(($) => $.plugins.remote_mcp.auth_types[value])}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </label>
        ) : null}
        <label className="space-y-1 text-caption">
          <span className="font-medium">{t(($) => $.plugins.remote_mcp.failure_policy)}</span>
          <Select
            items={(["required", "optional"] as const).map((value) => ({
              value,
              label: t(($) => $.plugins.remote_mcp.failure_policies[value]),
            }))}
            value={failurePolicy}
            onValueChange={(value) => value && setFailurePolicy(value)}
            disabled={!canManage || isPending}
          >
            <SelectTrigger><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="required">{t(($) => $.plugins.remote_mcp.failure_policies.required)}</SelectItem>
              <SelectItem value="optional">{t(($) => $.plugins.remote_mcp.failure_policies.optional)}</SelectItem>
            </SelectContent>
          </Select>
        </label>
        {authType === "header" ? (
          <label className="space-y-1 text-caption">
            <span className="font-medium">{t(($) => $.plugins.remote_mcp.header_name)}</span>
            <Input value={authHeader} onChange={(event) => setAuthHeader(event.target.value)} disabled={!canManage || isPending} />
          </label>
        ) : null}
        {authType === "bearer" || authType === "header" ? (
          <label className="space-y-1 text-caption">
            <span className="font-medium">{t(($) => $.plugins.remote_mcp.credential)}</span>
            <Input
              type="password"
              autoComplete="new-password"
              value={credential}
              onChange={(event) => setCredential(event.target.value)}
              disabled={!canManage || isPending}
            />
          </label>
        ) : null}
        {authType === "oauth" ? (
          <>
            <label className="space-y-1 text-caption sm:col-span-2">
              <span className="font-medium">{t(($) => $.plugins.remote_mcp.oauth_scope)}</span>
              <Input value={oauthScope} onChange={(event) => setOAuthScope(event.target.value)} disabled={!canManage || isPending} />
            </label>
            <label className="space-y-1 text-caption">
              <span className="font-medium">{t(($) => $.plugins.remote_mcp.oauth_client_id)}</span>
              <Input value={oauthClientId} onChange={(event) => setOAuthClientId(event.target.value)} disabled={!canManage || isPending} />
            </label>
            <label className="space-y-1 text-caption">
              <span className="font-medium">{t(($) => $.plugins.remote_mcp.oauth_client_secret)}</span>
              <Input type="password" autoComplete="new-password" value={oauthClientSecret} onChange={(event) => setOAuthClientSecret(event.target.value)} disabled={!canManage || isPending} />
            </label>
            <label className="space-y-1 text-caption">
              <span className="font-medium">{t(($) => $.plugins.remote_mcp.oauth_authorization_endpoint)}</span>
              <Input value={oauthAuthorizationEndpoint} onChange={(event) => setOAuthAuthorizationEndpoint(event.target.value)} disabled={!canManage || isPending} />
            </label>
            <label className="space-y-1 text-caption">
              <span className="font-medium">{t(($) => $.plugins.remote_mcp.oauth_token_endpoint)}</span>
              <Input value={oauthTokenEndpoint} onChange={(event) => setOAuthTokenEndpoint(event.target.value)} disabled={!canManage || isPending} />
            </label>
            {oauthClientSecret ? (
              <label className="space-y-1 text-caption sm:col-span-2">
                <span className="font-medium">{t(($) => $.plugins.remote_mcp.oauth_token_auth_method)}</span>
                <Select
                  items={(["none", "client_secret_basic", "client_secret_post"] as const).map((value) => ({ value, label: value }))}
                  value={oauthTokenAuthMethod}
                  onValueChange={(value) => value && setOAuthTokenAuthMethod(value)}
                  disabled={!canManage || isPending}
                >
                  <SelectTrigger><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">{t(($) => $.plugins.remote_mcp.oauth_token_auth_methods.none)}</SelectItem>
                    <SelectItem value="client_secret_basic">{t(($) => $.plugins.remote_mcp.oauth_token_auth_methods.client_secret_basic)}</SelectItem>
                    <SelectItem value="client_secret_post">{t(($) => $.plugins.remote_mcp.oauth_token_auth_methods.client_secret_post)}</SelectItem>
                  </SelectContent>
                </Select>
              </label>
            ) : null}
          </>
        ) : null}
        <label className="space-y-1 text-caption sm:col-span-2">
          <span className="font-medium">{t(($) => $.plugins.remote_mcp.public_config)}</span>
          <Textarea
            className="font-mono"
            value={publicConfig}
            onChange={(event) => setPublicConfig(event.target.value)}
            disabled={!canManage || isPending}
          />
        </label>
      </div>
      <div className="border-t border-surface-border p-3">
        <Button
          size="sm"
          disabled={!canManage || isPending || endpoint.trim() === "" || ((authType === "bearer" || authType === "header") && credential === "")}
          onClick={() => void configure()}
        >
          {isPending ? <Loader2 className="animate-spin" /> : null}
          {t(($) => $.plugins.remote_mcp.configure_and_discover)}
        </Button>
      </div>
    </details>
  );
}
