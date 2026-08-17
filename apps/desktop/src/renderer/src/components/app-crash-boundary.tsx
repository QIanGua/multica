import type { ReactNode } from "react";
import { ErrorBoundary } from "@multica/ui/components/common/error-boundary";
import { Button } from "@multica/ui/components/ui/button";
import { captureEvent } from "@multica/core/analytics";

/**
 * Last-resort boundary around the entire desktop renderer.
 *
 * The renderer had no boundary above the shell at all: `createRoot().render()`
 * mounted <App /> bare, and the router's `errorElement` only covers what the
 * router itself renders — the sidebar, search dialog, modal registry and
 * window overlay are siblings of the router, outside its reach. A render-time
 * throw in any of them unmounted the whole React tree and left an empty,
 * unresponsive window with no way back except force-quitting the app. That is
 * exactly what #7021 reported after deleting the last workspace (MUL-6231).
 *
 * The specific throw behind that report is fixed at its source, but "one
 * component throws" must not stay a whole-app kill switch on desktop, where
 * there is no reload button and no URL bar to escape with. This turns the
 * blank window into a readable error plus a Reload button.
 *
 * Deliberately NOT a `reset()` fallback: whatever state produced the throw is
 * still in the stores, so re-rendering the same tree usually re-throws.
 * A full `location.reload()` re-runs bootstrap from persisted state, which is
 * the recovery path the user would otherwise get by restarting the app.
 */
export function AppCrashBoundary({ children }: { children: ReactNode }) {
  return (
    <ErrorBoundary
      onError={(error) => {
        captureEvent("desktop_renderer_crash", {
          message: error.message,
          stack: error.stack?.slice(0, 2000),
        });
      }}
      fallback={({ error }) => <CrashFallback error={error} />}
    >
      {children}
    </ErrorBoundary>
  );
}

function CrashFallback({ error }: { error: Error }) {
  return (
    <div
      role="alert"
      className="flex h-screen items-center justify-center bg-background p-8 text-foreground"
    >
      <div className="max-w-xl rounded-lg border bg-card p-6 shadow-sm">
        <h1 className="text-title font-semibold">Something went wrong</h1>
        <p className="mt-3 text-body text-muted-foreground">
          Multica Desktop hit an unexpected error and could not keep rendering.
          Reloading usually recovers — your work is stored on the server.
        </p>
        <pre className="mt-4 max-h-48 overflow-auto whitespace-pre-wrap rounded-md bg-muted p-3 text-caption text-muted-foreground">
          {error.message || "An unexpected error occurred."}
        </pre>
        <Button className="mt-4" onClick={() => window.location.reload()}>
          Reload
        </Button>
      </div>
    </div>
  );
}
