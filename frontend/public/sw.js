// Talyvor Docs service worker.
//
// Two caches:
//   talyvor-static-v2  → the shell is NETWORK-FIRST; /assets/* is cache-first (immutable)
//   talyvor-api-v2     → network-first for cacheable GET API responses
//
// Writes (POST/PATCH/PUT/DELETE) are never cached here — the IndexedDB
// write_queue in the React layer handles offline replay. The service
// worker only proxies reads.
//
// SHELL STRATEGY — WHY NETWORK-FIRST (this is the fix for the stale-SW routing hazard):
// index.html is NOT content-hashed (always served at "/"), and it is the file that pulls in
// the hashed JS/CSS bundles. Caching it CACHE-FIRST pins the entire app version: after a new
// deploy a returning user was served the old index.html forever (cache-first never
// revalidates), which loaded the old bundle from cache too — a stale app mismatched with the
// fresh server, which manifested as erratic client-side routing until the SW was manually
// unregistered. Serving the shell NETWORK-FIRST keeps returning online users on the current
// version; the cached shell is the OFFLINE fallback only. Hashed /assets/* stay cache-first
// because they are immutable, so they are safe (and fast) to pin.

// Bumping the version (v1 -> v2) makes the new SW's activate purge the old, poisoned
// talyvor-static-v1 cache once, healing an existing stale install. Because the shell is now
// network-first, future deploys self-propagate WITHOUT another bump.
const CACHE_VERSION = "v2";
const STATIC_CACHE = `talyvor-static-${CACHE_VERSION}`;
const API_CACHE = `talyvor-api-${CACHE_VERSION}`;

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
  // hard-coding hashed filenames. skipWaiting so a fixed SW activates
  // promptly instead of waiting for every tab to close.
  self.skipWaiting();
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    (async () => {
      const keys = await caches.keys();
      // Drop any cache whose name isn't a current-version cache — this is what purges the
      // old talyvor-*-v1 caches (including the poisoned stale-shell one) on the fix deploy.
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

  // Hashed static assets are immutable → cache-first (correct and fast).
  if (url.pathname.startsWith("/assets/")) {
    event.respondWith(cacheFirst(req));
    return;
  }

  // The app SHELL → network-first. See the header comment: cache-first here is the bug.
  if (url.pathname === "/" || url.pathname === "/index.html") {
    event.respondWith(shellNetworkFirst(req));
    return;
  }

  // API GETs (everything under /v1/ that isn't search/collab) get
  // network-first with a cached fallback.
  if (url.pathname.startsWith("/v1/")) {
    event.respondWith(networkFirst(req));
    return;
  }

  // Everything else (deep client routes like /spaces/.., /domains) passes through to the
  // browser → server SPA fallback. The SW deliberately does NOT cache-serve these — that is
  // what turned into the routing hijack.
});

// cacheFirst is for IMMUTABLE hashed assets only. A cache hit is authoritative (the content
// hash guarantees it can't be stale); only a miss goes to the network.
async function cacheFirst(req) {
  const cache = await caches.open(STATIC_CACHE);
  const hit = await cache.match(req);
  if (hit) return hit;
  try {
    const res = await fetch(req);
    if (res.ok) cache.put(req, res.clone()).catch(() => {});
    return res;
  } catch {
    return new Response("", { status: 504, statusText: "Offline" });
  }
}

// shellNetworkFirst serves the app shell fresh when online, and falls back to the cached
// shell when offline so the SPA still mounts. This is the strategy that makes a deploy
// visible to returning users instead of pinning a stale version.
async function shellNetworkFirst(req) {
  const cache = await caches.open(STATIC_CACHE);
  try {
    const res = await fetch(req);
    // Refresh the cached shell so the OFFLINE fallback tracks the latest deploy too.
    if (res.ok) cache.put(req, res.clone()).catch(() => {});
    return res;
  } catch {
    const hit =
      (await cache.match(req)) ||
      (req.mode === "navigate" ? await cache.match("/") : null);
    if (hit) return hit;
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
