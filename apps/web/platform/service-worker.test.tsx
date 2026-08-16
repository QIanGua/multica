import { afterEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import { ServiceWorkerRegistrar, SERVICE_WORKER_URL } from "./service-worker";

function stubServiceWorker(register: ReturnType<typeof vi.fn>) {
  Object.defineProperty(navigator, "serviceWorker", {
    value: { register },
    configurable: true,
    writable: true,
  });
}

afterEach(() => {
  Reflect.deleteProperty(navigator, "serviceWorker");
});

describe("ServiceWorkerRegistrar", () => {
  it("registers the worker at the root scope", () => {
    const register = vi.fn().mockResolvedValue({});
    stubServiceWorker(register);

    render(<ServiceWorkerRegistrar />);

    expect(register).toHaveBeenCalledWith(SERVICE_WORKER_URL, { scope: "/" });
  });

  it("does nothing when the browser has no service worker support", () => {
    expect(() => render(<ServiceWorkerRegistrar />)).not.toThrow();
  });

  it("swallows a rejected registration", async () => {
    // Private windows and some enterprise storage policies reject outright;
    // the app works fine without a worker, so this must not surface. Drop the
    // component's .catch() and the flush below turns the rejection into an
    // unhandled one, which fails the run.
    const register = vi.fn().mockRejectedValue(new Error("blocked"));
    stubServiceWorker(register);

    render(<ServiceWorkerRegistrar />);
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(register).toHaveBeenCalledTimes(1);
  });
});
