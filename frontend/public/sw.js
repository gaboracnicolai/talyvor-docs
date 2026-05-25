// Talyvor Docs service worker.
//
// Two caches:
//   talyvor-static-v1  → cache-first for /assets/* (hashed, immutable)
//   talyvor-api-v1     → network-first for cacheable GET API responses
//
// Writes (POST/PATCH/PUT/DELETE) are never cached here — the IndexedDB
// write_queue in the React layer handles offline replay. The service
// worker only proxies reads.

const STATIC_CACHE = "talyvor-static-v1";
const API_CACHE = "talyvor-api-v1";

// Patterns that should bypass the cache entirely. Real-time
// surfaces (collab WS, MCP, search) shouldn't return stale
// snapshots — better to fail and let the React layer surface an
// offline state than to lie.
const SKIP_PATTERNS = [
  /\/v1\/collab\//,
  /^\/mcp(\/|$)/,
  /\/v1\/workspaces\/[^/]+\/search\b/,
];

self.addEventListener("install", (event) => {
  // No pre-cache list — the static cache fills lazily as users hit
  // each asset. This keeps the install step fast and avoids
  // hard-coding hashed filenames.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const keys = await caches.keys();
      // Drop any older cache versions so a deploy doesn't accumulate
      // stale entries indefinitely.
      await Promise.all(
        keys
          .filter((k) => k !== STATIC_CACHE && k !== API_CACHE)
          .map((k) => caches.delete(k)),
      );
      await self.clients.claim();
    })(),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.method !== "GET") {
    // Non-GET requests pass through untouched. The React layer's
    // write queue handles offline POST/PATCH/DELETE replay.
    return;
  }
  const url = new URL(req.url);

  // Same-origin only — the SW intentionally doesn't proxy
  // third-party resources (lens.talyvor.com, track.talyvor.com,
  // etc.) because we'd just leak their failure modes through.
  if (url.origin !== self.location.origin) return;

  if (SKIP_PATTERNS.some((re) => re.test(url.pathname))) {
    return;
  }

  // Hashed static assets get cache-first treatment. Vite emits them
  // with content hashes, so once cached they're immutable.
  if (url.pathname.startsWith("/assets/") || url.pathname === "/index.html" || url.pathname === "/") {
    event.respondWith(cacheFirst(req));
    return;
  }

  // API GETs (everything under /v1/ that isn't search/collab) get
  // network-first with a cached fallback.
  if (url.pathname.startsWith("/v1/")) {
    event.respondWith(networkFirst(req));
    return;
  }
});

async function cacheFirst(req) {
  const cache = await caches.open(STATIC_CACHE);
  const hit = await cache.match(req);
  if (hit) return hit;
  try {
    const res = await fetch(req);
    if (res.ok) cache.put(req, res.clone()).catch(() => {});
    return res;
  } catch {
    // Last-ditch: serve the cached index.html for navigation
    // requests so the SPA at least mounts when fully offline.
    if (req.mode === "navigate") {
      const fallback = await cache.match("/");
      if (fallback) return fallback;
    }
    return new Response("", { status: 504, statusText: "Offline" });
  }
}

async function networkFirst(req) {
  const cache = await caches.open(API_CACHE);
  try {
    const res = await fetch(req);
    if (res.ok) {
      // Cache a clone so the response body is still consumable by
      // the original caller.
      cache.put(req, res.clone()).catch(() => {});
    }
    return res;
  } catch {
    const hit = await cache.match(req);
    if (hit) {
      // Mark the cached response so the client can show a "Cached"
      // indicator. We add a header without re-encoding the body.
      const headers = new Headers(hit.headers);
      headers.set("X-From-Cache", "true");
      return new Response(await hit.clone().blob(), {
        status: hit.status,
        statusText: hit.statusText,
        headers,
      });
    }
    return new Response(
      JSON.stringify({ error: "offline", offline: true }),
      {
        status: 503,
        headers: { "Content-Type": "application/json", "X-Offline": "true" },
      },
    );
  }
}
