"use client";

import { useEffect, useId, useMemo, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { pluginSurfaceLaunchOptions } from "@multica/core/plugins";
import type { PluginInstallation, PluginSurface } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import { buildSurfaceFrameDocument, readThemeTokens } from "./surface-document";
import { createSurfaceBridge } from "./surface-bridge";

const DEFAULT_HEIGHT = 220;

interface PluginSurfaceFrameProps {
  wsId: string;
  installation: PluginInstallation;
  surface: PluginSurface;
  issueId?: string;
  className?: string;
}

/**
 * Mounts one plugin surface.
 *
 * This element is a TRUSTED wrapper. Its nested plugin iframe is the isolation
 * boundary and is always `sandbox="allow-scripts"` without
 * `allow-same-origin`; see buildSurfaceFrameDocument.
 */
export function PluginSurfaceFrame({ wsId, installation, surface, issueId, className }: PluginSurfaceFrameProps) {
  const frameRef = useRef<HTMLIFrameElement>(null);
  const anchorRef = useRef<HTMLDivElement>(null);
  const [height, setHeight] = useState(DEFAULT_HEIGHT);
  const [failed, setFailed] = useState(false);
  const [navigated, setNavigated] = useState(false);
  const launchInstance = useId();

  // Every mounted frame gets its own launch. The artifact is immutable, but the
  // bridge proof is deliberately neither cacheable nor shareable.
  const { data: launch, isPending, isError } = useQuery(
    pluginSurfaceLaunchOptions(wsId, installation.id, surface.key, installation.package_version_id, launchInstance, issueId),
  );

  const surfaceDocument = useMemo(() => {
    if (!launch?.url || !launch.bridge_token) return null;
    try {
      return buildSurfaceFrameDocument({ url: launch.url, bridgeToken: launch.bridge_token });
    } catch {
      return null;
    }
  }, [launch?.url, launch?.bridge_token]);

  const bridge = useMemo(
    () => createSurfaceBridge({
      installationId: installation.id,
      bridgeToken: launch?.bridge_token ?? "",
      issueId,
      onResize: setHeight,
    }),
    [installation.id, launch?.bridge_token, issueId],
  );

  // The listener is armed BEFORE srcdoc is assigned. That makes the guest-first
  // one-shot port transfer race-free without retries or a reusable token.
  useEffect(() => {
    const frame = frameRef.current;
    if (!frame || !surfaceDocument) return () => bridge.close();
    setFailed(false);
    setNavigated(false);
    bridge.connect(frame, readThemeTokens(anchorRef.current));
    frame.srcdoc = surfaceDocument;
    return () => {
      bridge.close();
      frame.removeAttribute("srcdoc");
    };
  }, [bridge, surfaceDocument]);

  useEffect(() => {
    const onMessage = (event: MessageEvent) => {
      const type = (event.data as { type?: string } | null)?.type;
      if (type !== "multica:plugin-surface-error" &&
          type !== "multica:plugin-surface-navigated" &&
          type !== "multica:plugin-surface-navigation-blocked") return;
      // Same window-identity rule as the bridge: without it any frame on the
      // page could light up the failure banner on every other panel.
      if (!frameRef.current?.contentWindow || event.source !== frameRef.current.contentWindow) return;
      // A surface whose script throws on its first line posts the error rather
      // than rendering blank — the frame has no other way to tell us.
      if (type === "multica:plugin-surface-error") return setFailed(true);
      setNavigated(true);
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  useEffect(() => {
    if (navigated) bridge.close();
  }, [navigated, bridge]);

  if (navigated) {
    return (
      <div ref={anchorRef} className={cn("rounded-lg border border-surface-border px-4 py-3 text-caption text-muted-foreground", className)}>
        <PluginSurfaceNotice installation={installation} kind="navigated" />
      </div>
    );
  }

  if (!surfaceDocument) {
    return (
      <div ref={anchorRef} className={cn("rounded-lg border border-surface-border px-4 py-3 text-caption text-muted-foreground", className)}>
        {/* Three states share this box on purpose: still loading, the request
            failed, and the installed version carries no code for this surface.
            All three mean "nothing to render yet"; only the last is permanent,
            and none of them should show an empty frame the reader cannot act
            on. */}
        <PluginSurfaceNotice installation={installation} kind={isPending && !isError ? "loading" : "unavailable"} />
      </div>
    );
  }

  return (
    <div ref={anchorRef} className={cn("overflow-hidden rounded-lg border border-surface-border", className)}>
      {failed ? (
        <div className="px-4 py-3 text-caption text-muted-foreground">
          <PluginSurfaceNotice installation={installation} kind="failed" />
        </div>
      ) : null}
      <iframe
        // Keyed on the issue as well: a new bridge is created when issueId
        // changes, but an unchanged document would not reload, and the guest
        // stops announcing once answered — the fresh bridge would wait forever.
        key={`${installation.id}:${surface.key}:${issueId ?? ""}`}
        ref={frameRef}
        title={`${installation.name} — ${surface.name}`}
        // This is the host-authored wrapper, so same-origin is intentional. Its
        // nested plugin iframe remains opaque and owns no app credential.
        sandbox="allow-scripts allow-same-origin"
        className="w-full border-0 bg-transparent"
        style={{ height }}
      />
    </div>
  );
}

function PluginSurfaceNotice({
  installation,
  kind,
}: {
  installation: PluginInstallation;
  kind: "unavailable" | "failed" | "loading" | "navigated";
}) {
  const { t } = useT("issues");
  if (kind === "failed") return <>{t(($) => $.plugins.surface_failed, { name: installation.name })}</>;
  if (kind === "loading") return <>{t(($) => $.plugins.surface_loading, { name: installation.name })}</>;
  if (kind === "navigated") return <>{t(($) => $.plugins.surface_navigated, { name: installation.name })}</>;
  return <>{t(($) => $.plugins.surface_unavailable, { name: installation.name })}</>;
}
