import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen } from "@testing-library/react";

const captureEvent = vi.hoisted(() => vi.fn());
vi.mock("@multica/core/analytics", () => ({ captureEvent }));

import { AppCrashBoundary } from "./app-crash-boundary";

function Boom(): never {
  throw new Error("useWorkspaceId: no workspace selected");
}

beforeEach(() => {
  captureEvent.mockClear();
  // The boundary logs the captured error on purpose; keep the suite output
  // readable without hiding a genuinely unexpected console.error elsewhere.
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  vi.restoreAllMocks();
});

/**
 * MUL-6231 / #7021. The desktop renderer mounted <App /> with no boundary
 * above it, so one throw in the shell emptied the window and left force-quit
 * as the only way out.
 */
describe("AppCrashBoundary", () => {
  it("renders children when nothing throws", () => {
    render(
      <AppCrashBoundary>
        <div data-testid="app" />
      </AppCrashBoundary>,
    );

    expect(screen.queryByTestId("app")).not.toBeNull();
  });

  it("shows a recoverable fallback instead of blanking the window", () => {
    render(
      <AppCrashBoundary>
        <Boom />
      </AppCrashBoundary>,
    );

    const alert = screen.getByRole("alert");
    expect(alert).not.toBeNull();
    expect(alert.textContent).toContain("useWorkspaceId: no workspace selected");
    expect(screen.getByRole("button", { name: /reload/i })).not.toBeNull();
  });

  it("reports the crash so a blank-window regression is visible in telemetry", () => {
    render(
      <AppCrashBoundary>
        <Boom />
      </AppCrashBoundary>,
    );

    expect(captureEvent).toHaveBeenCalledWith(
      "desktop_renderer_crash",
      expect.objectContaining({
        message: "useWorkspaceId: no workspace selected",
      }),
    );
  });
});
