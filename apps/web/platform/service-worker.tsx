"use client";

import { useEffect } from "react";

/** Path of the worker served from public/. Scope follows from its location. */
export const SERVICE_WORKER_URL = "/sw.js";

/**
 * Registers the service worker that makes the app installable.
 *
 * Mounted once from the root layout, which also owns the development gate —
 * this component registers unconditionally. Renders nothing; the worker itself
 * does nothing at runtime either (see public/sw.js). Its only job is to satisfy
 * Chrome's installability check so Android offers "Install app", and to give
 * later caching work a worker that already updates safely.
 */
export function ServiceWorkerRegistrar() {
  useEffect(() => {
    if (typeof navigator === "undefined" || !("serviceWorker" in navigator))
      return;

    // Failure is not worth surfacing: registration is blocked outright in
    // private windows and by some enterprise storage policies, and the app is
    // fully functional without a worker.
    void navigator.serviceWorker
      .register(SERVICE_WORKER_URL, { scope: "/" })
      .catch(() => {});
  }, []);

  return null;
}
