"use client";

import { useEffect, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { getApi } from "../api";
import { ApiError } from "../api/client";
import { useAuthStore } from "../auth";
import {
  captureSignupSource,
  identify as identifyAnalytics,
  initAnalytics,
  resetAnalytics,
} from "../analytics";
import { configStore } from "../config";
import {
  workspaceKeys,
  workspaceListOptions,
} from "../workspace/queries";
import { createLogger } from "../logger";
import { defaultStorage } from "./storage";
import { setCurrentWorkspace } from "./workspace-storage";
import type { ClientIdentity } from "./types";
import type { StorageAdapter } from "../types/storage";
import type { User } from "../types";

const logger = createLogger("auth");
const AUTH_RETRY_DELAYS_MS = [1_000, 2_000, 4_000, 8_000, 16_000] as const;
const noopRetry = () => {};

export function AuthInitializer({
  children,
  onLogin,
  onLogout,
  storage = defaultStorage,
  cookieAuth,
  identity,
}: {
  children: ReactNode;
  onLogin?: () => void;
  onLogout?: () => void;
  storage?: StorageAdapter;
  cookieAuth?: boolean;
  identity?: ClientIdentity;
}) {
  const qc = useQueryClient();
  const [retryGeneration, setRetryGeneration] = useState(0);

  useEffect(() => {
    const api = getApi();

    // Stamp attribution before anything else — the signup event (server-side)
    // reads this cookie, so it has to be present before the user hits submit.
    captureSignupSource();

    // Fetch app config (CDN domain, PostHog key, …) in the background — non-blocking.
    api
      .getConfig()
      .then((cfg) => {
        if (cfg.cdn_domain) {
          configStore.getState().setCdnConfig({
            cdnDomain: cfg.cdn_domain,
            // Old servers omit this — false keeps the previous behavior.
            cdnSigned: cfg.cdn_signed === true,
          });
        }
        configStore.getState().setAuthConfig({
          allowSignup: cfg.allow_signup,
          googleClientId: cfg.google_client_id,
          // Old servers omit this field — treat that as "creation allowed"
          // (the managed-cloud default) rather than blocking the UI.
          workspaceCreationDisabled: cfg.workspace_creation_disabled === true,
          // Absent/false on the managed cloud and older servers → section hidden.
          vcsIntegrationAvailable: cfg.vcs_integration_available === true,
        });
        configStore.getState().setDaemonConfig({
          daemonServerUrl: cfg.daemon_server_url,
          daemonAppUrl: cfg.daemon_app_url,
        });
        configStore.getState().setFeatureFlags(cfg.feature_flags);
        configStore.getState().setServerVersion(cfg.server_version);
        if (cfg.posthog_key) {
          initAnalytics({
            key: cfg.posthog_key,
            host: cfg.posthog_host || "",
            appVersion: identity?.version,
            environment: cfg.analytics_environment,
          });
        }
      })
      .catch(() => {
        /* config is optional — legacy file card matching degrades gracefully */
      });
  // Configuration is boot-scoped; auth retries must not restart this request.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    const retry = () => {
      useAuthStore.setState({
        isLoading: true,
        status: "authenticating",
      });
      setRetryGeneration((generation) => generation + 1);
    };
    useAuthStore.setState({ retryAuthentication: retry });

    return () => {
      if (useAuthStore.getState().retryAuthentication === retry) {
        useAuthStore.setState({ retryAuthentication: noopRetry });
      }
    };
  }, []);

  useEffect(() => {
    const api = getApi();
    let cancelled = false;
    let settled = false;
    let inFlight = false;
    let retryAfterFlight = false;
    let retryIndex = 0;
    let retryTimer: ReturnType<typeof setTimeout> | undefined;

    const onAuthSuccess = (user: User) => {
      onLogin?.();
      useAuthStore.setState({
        user,
        isLoading: false,
        status: "authenticated",
      });
      identifyAnalytics(user.id, { email: user.email, name: user.name });
    };

    const onAuthFailure = () => {
      onLogout?.();
      resetAnalytics();
      useAuthStore.setState({
        user: null,
        isLoading: false,
        status: "unauthenticated",
      });
    };

    const rejectSession = () => {
      settled = true;
      if (retryTimer !== undefined) clearTimeout(retryTimer);
      if (!cookieAuth) {
        setCurrentWorkspace(null, null);
      }
      onAuthFailure();
    };

    const scheduleRetry = (attempt: () => Promise<void>) => {
      if (cancelled || settled || retryIndex >= AUTH_RETRY_DELAYS_MS.length) {
        return;
      }
      const delay = AUTH_RETRY_DELAYS_MS[retryIndex];
      retryIndex += 1;
      retryTimer = setTimeout(() => {
        retryTimer = undefined;
        void attempt();
      }, delay);
    };

    const warmDesktopWorkspaces = () => {
      void qc.fetchQuery(workspaceListOptions()).catch((err: unknown) => {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          rejectSession();
          return;
        }
        // The authenticated desktop shell observes this query directly.
        // React Query owns its retry and refetch-on-reconnect behavior.
        logger.error("desktop workspace bootstrap failed", err);
      });
    };

    const attempt = async (): Promise<void> => {
      if (cancelled || settled || inFlight) return;
      if (retryTimer !== undefined) {
        clearTimeout(retryTimer);
        retryTimer = undefined;
      }
      inFlight = true;

      try {
        const user = await api.getMe();
        if (cancelled) return;

        if (identity?.platform === "desktop") {
          // Desktop consumers own the workspace-list query and explicitly
          // gate destructive tab/overlay decisions on query success. Publish
          // the verified user immediately, then let React Query recover the
          // independent workspace request.
          settled = true;
          onAuthSuccess(user);
          warmDesktopWorkspaces();
          return;
        }

        // Web routes currently rely on this cache being seeded before auth is
        // published. Preserve that ordering while still treating a transient
        // workspace failure as recoverable rather than as a logout.
        const wsList = await api.listWorkspaces();
        if (cancelled) return;
        qc.setQueryData(workspaceKeys.list(), wsList);
        settled = true;
        onAuthSuccess(user);
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiError && err.status === 401) {
          rejectSession();
          return;
        }

        logger.error("auth init temporarily unavailable", err);
        useAuthStore.setState({
          user: null,
          isLoading: true,
          status: "recovering",
        });
        scheduleRetry(attempt);
      } finally {
        inFlight = false;
        if (retryAfterFlight && !cancelled && !settled) {
          retryAfterFlight = false;
          void attempt();
        }
      }
    };

    const retryNow = () => {
      if (cancelled || settled) return;
      retryIndex = 0;
      if (retryTimer !== undefined) {
        clearTimeout(retryTimer);
        retryTimer = undefined;
      }
      if (inFlight) {
        retryAfterFlight = true;
        return;
      }
      void attempt();
    };

    window.addEventListener("online", retryNow);

    if (!cookieAuth) {
      const token = storage.getItem("multica_token");
      if (!token) {
        settled = true;
        onLogout?.();
        useAuthStore.setState({
          user: null,
          isLoading: false,
          status: "unauthenticated",
        });
      } else {
        api.setToken(token);
        void attempt();
      }
    } else {
      void attempt();
    }

    return () => {
      cancelled = true;
      window.removeEventListener("online", retryNow);
      if (retryTimer !== undefined) clearTimeout(retryTimer);
    };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [retryGeneration]);

  return <>{children}</>;
}
