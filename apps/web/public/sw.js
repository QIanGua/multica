/*
 * Multica service worker.
 *
 * This worker deliberately caches NOTHING. It exists for two reasons:
 *
 * 1. Chrome only offers to install a site whose active worker has a fetch
 *    handler, so without one the app is installable on iOS (which just needs
 *    the manifest plus the apple-* meta tags) but not on Android.
 * 2. It establishes the update discipline up front. A worker that starts
 *    serving HTML from a cache later is the classic way to strand users on a
 *    stale document requesting `/_next` chunks the current deploy has already
 *    deleted. Having skipWaiting + clients.claim + a cache sweep in place
 *    before any caching exists means that change only has to add a strategy,
 *    not retrofit a safe upgrade path.
 *
 * Caching, an offline fallback document and push handling are explicitly out
 * of scope here — see MUL-6237 for what the later tiers add.
 *
 * Served verbatim from public/, so any edit to this file is what tells a
 * browser to fetch and activate a new worker.
 */

self.addEventListener("install", () => {
  // Nothing to precache, so take over as soon as the browser allows rather
  // than waiting for every tab to close.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      // This revision owns no caches. Anything still on disk belongs to an
      // earlier worker, so drop it instead of leaving orphaned storage behind.
      const keys = await caches.keys();
      await Promise.all(keys.map((key) => caches.delete(key)));
      await self.clients.claim();
    })(),
  );
});

self.addEventListener("fetch", () => {
  // Intentionally empty: nothing calls respondWith, so every request goes to
  // the network exactly as it would without a worker. Do not add caching here
  // without also adding a versioned cache name and matching eviction in the
  // `activate` sweep above.
});
