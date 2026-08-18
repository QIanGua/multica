"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { PluginInstallation, PluginSurface } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../i18n";
import { buildSurfaceDocument, readThemeTokens, resolveSurfaceEntry } from "./surface-document";
import { createSurfaceBridge } from "./surface-bridge";

const DEFAULT_HEIGHT = 220;

interface PluginSurfaceFrameProps {
  installation: PluginInstallation;
  surface: PluginSurface;
  issueId?: string;
  className?: string;
}

/**
 * Mounts one plugin surface.
 *
 * `sandbox="allow-scripts"` WITHOUT `allow-same-origin` is the whole isolation
 * boundary and must never be loosened: the pairing is called out in the HTML
 * spec as defeating the sandbox, and it is what would hand a third-party script
 * our cookies and storage. Same rule, same reason as
 * `packages/views/editor/code-block-iframe.tsx`.
 */
export function PluginSurfaceFrame({ installation, surface, issueId, className }: PluginSurfaceFrameProps) {
  const frameRef = useRef<HTMLIFrameElement>(null);
  const anchorRef = useRef<HTMLDivElement>(null);
  const [height, setHeight] = useState(DEFAULT_HEIGHT);
  const [failed, setFailed] = useState(false);

  const entryUrl = useMemo(() => resolveSurfaceEntry(installation, surface), [installation, surface]);

  // Read the tokens off a real element so the frame inherits whatever theme the
  // app is currently in, rather than a hardcoded copy that drifts.
  const theme = useMemo(() => readThemeTokens(anchorRef.current), []);

  const document = useMemo(() => {
    if (!entryUrl) return null;
    return buildSurfaceDocument({ entryUrl, grantedScopes: installation.granted_scopes, theme });
  }, [entryUrl, installation.granted_scopes, theme]);

  const bridge = useMemo(
    () => createSurfaceBridge({ installationId: installation.id, issueId, onResize: setHeight }),
    [installation.id, issueId],
  );

  useEffect(() => () => bridge.close(), [bridge]);

  const onLoad = useCallback(() => {
    if (frameRef.current) bridge.connect(frameRef.current, readThemeTokens(anchorRef.current));
  }, [bridge]);

  useEffect(() => {
    // A surface whose script 404s posts this to the parent window rather than
    // rendering blank — the frame has no other way to tell us.
    const onMessage = (event: MessageEvent) => {
      if ((event.data as { type?: string } | null)?.type === "multica:plugin-surface-error") setFailed(true);
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  if (!document) {
    return (
      <div ref={anchorRef} className={cn("rounded-lg border border-surface-border px-4 py-3 text-caption text-muted-foreground", className)}>
        {/* A local: install has no URL to load a script from — say so instead of
            showing an empty box the user cannot act on. */}
        <PluginSurfaceNotice installation={installation} kind="unavailable" />
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
        ref={frameRef}
        title={`${installation.name} — ${surface.name}`}
        srcDoc={document}
        sandbox="allow-scripts"
        onLoad={onLoad}
        className="w-full border-0 bg-transparent"
        style={{ height }}
      />
    </div>
  );
}

function PluginSurfaceNotice({ installation, kind }: { installation: PluginInstallation; kind: "unavailable" | "failed" }) {
  const { t } = useT("issues");
  if (kind === "failed") return <>{t(($) => $.plugins.surface_failed, { name: installation.name })}</>;
  return <>{t(($) => $.plugins.surface_local_only, { name: installation.name })}</>;
}
