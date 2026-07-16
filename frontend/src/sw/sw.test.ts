import { describe, expect, it, vi, beforeEach } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Tests the REAL shipped service worker — public/sw.js is loaded into a mocked
// ServiceWorkerGlobalScope and its handlers are exercised directly. No drift: this is the
// exact file that deploys, not a re-implementation. The root hazard (a cache-first app shell
// serving a stale index.html across deploys) is a behaviour of THIS file, so THIS file is
// what the test pins.
//
// The discriminator between cache-first and network-first, given a cache HIT for the shell:
//   cache-first  → returns the cached hit and does NOT call fetch.
//   network-first→ calls fetch first (fresh when online), cache only as a fallback.
// So "did the handler call fetch for a navigate to / when the cache already has /" is the
// exact fix, expressed as behaviour.

const swPath = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../public/sw.js");
const swSource = readFileSync(swPath, "utf8");

const ORIGIN = "https://app.talyvor.test";

// A mock CacheStorage. Content is a single shared map (so seeding a hit is name-agnostic);
// cache NAMES are tracked separately so the activate-purge test can assert version cleanup.
function makeCaches() {
  const content = new Map<string, Response>();
  const names = new Set<string>();
  const keyOf = (req: Request | string) => (typeof req === "string" ? req : req.url);
  const cacheFor = () => ({
    match: async (req: Request | string) => content.get(keyOf(req)) ?? undefined,
    put: async (req: Request | string, res: Response) => void content.set(keyOf(req), res),
  });
  return {
    _content: content,
    _names: names,
    open: async (name: string) => {
      names.add(name);
      return cacheFor();
    },
    keys: async () => [...names],
    delete: async (name: string) => names.delete(name),
    match: async (req: Request | string) => content.get(keyOf(req)) ?? undefined,
  };
}

// Load the SW source with self/caches/fetch injected, capturing its event handlers.
function loadSW(caches: ReturnType<typeof makeCaches>, fetchImpl: typeof fetch) {
  const handlers: Record<string, (e: any) => void> = {};
  const self = {
    addEventListener: (type: string, fn: (e: any) => void) => void (handlers[type] = fn),
    skipWaiting: vi.fn(),
    clients: { claim: vi.fn() },
    location: { origin: ORIGIN },
    registration: {},
  };
  // eslint-disable-next-line no-new-func
  new Function("self", "caches", "fetch", swSource)(self, caches, fetchImpl);
  return { handlers, self };
}

// Drive the fetch handler with a synthetic FetchEvent and return what it respondWith'd
// (or null if it passed through without responding).
async function runFetch(
  handlers: Record<string, (e: any) => void>,
  url: string,
  opts: { mode?: string } = {},
): Promise<Response | null> {
  let responded: Promise<Response> | null = null;
  const event = {
    request: { url: ORIGIN + url, method: "GET", mode: opts.mode ?? "navigate" },
    respondWith: (p: Promise<Response>) => void (responded = p),
  };
  handlers.fetch(event);
  return responded ? await responded : null;
}

describe("public/sw.js — the shipped service worker", () => {
  let caches: ReturnType<typeof makeCaches>;
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    caches = makeCaches();
    fetchMock = vi.fn();
  });

  // THE ROOT-CAUSE FIX, as behaviour: the app shell must be served NETWORK-FIRST so a
  // returning user after a deploy gets the fresh index.html, not a pinned stale one.
  it("serves the app shell (/) NETWORK-FIRST — fetches fresh even when a stale / is cached", async () => {
    caches._content.set(ORIGIN + "/", new Response("STALE_SHELL"));
    fetchMock.mockResolvedValue(new Response("FRESH_SHELL"));

    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    const res = await runFetch(handlers, "/");

    expect(fetchMock).toHaveBeenCalled(); // network-first consults the network first
    expect(await res!.text()).toBe("FRESH_SHELL"); // and serves it, not the stale cache
  });

  it("serves /index.html network-first too (same shell, same rule)", async () => {
    caches._content.set(ORIGIN + "/index.html", new Response("STALE"));
    fetchMock.mockResolvedValue(new Response("FRESH"));

    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    const res = await runFetch(handlers, "/index.html");

    expect(fetchMock).toHaveBeenCalled();
    expect(await res!.text()).toBe("FRESH");
  });

  // Offline must still work: network-first falls back to the cached shell when the network
  // fails, so the SPA still mounts. (Preserving the intentional offline feature.)
  it("falls back to the cached shell when offline (offline feature preserved)", async () => {
    caches._content.set(ORIGIN + "/", new Response("CACHED_SHELL"));
    fetchMock.mockRejectedValue(new Error("offline"));

    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    const res = await runFetch(handlers, "/");

    expect(fetchMock).toHaveBeenCalled();
    expect(await res!.text()).toBe("CACHED_SHELL");
  });

  // Hashed assets are immutable, so cache-first is correct and must stay: a cached asset is
  // returned WITHOUT hitting the network.
  it("keeps /assets/* CACHE-FIRST — returns the cached asset without fetching", async () => {
    caches._content.set(ORIGIN + "/assets/index-abc123.js", new Response("CACHED_ASSET"));
    fetchMock.mockResolvedValue(new Response("SHOULD_NOT_BE_USED"));

    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    const res = await runFetch(handlers, "/assets/index-abc123.js", { mode: "no-cors" });

    expect(fetchMock).not.toHaveBeenCalled(); // cache hit → no network
    expect(await res!.text()).toBe("CACHED_ASSET");
  });

  // A deep client route (/spaces/..., /domains, ...) must NOT be intercepted by the SW — it
  // passes through to the browser → server SPA fallback. Intercepting it and serving a stale
  // shell is exactly the routing-hijack class this run fixes.
  it("passes deep client routes through untouched (no respondWith)", async () => {
    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    expect(await runFetch(handlers, "/domains")).toBeNull();
    expect(await runFetch(handlers, "/spaces/s1/pages/p1")).toBeNull();
  });

  // The version bump: activate must purge the OLD cache version so an existing poisoned
  // static cache (holding a stale shell) is cleared on the fix deploy.
  it("purges old cache versions on activate (heals an existing stale install)", async () => {
    caches._names.add("talyvor-static-v1");
    caches._names.add("talyvor-api-v1");

    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    let waited: Promise<unknown> | null = null;
    handlers.activate({ waitUntil: (p: Promise<unknown>) => void (waited = p) });
    await waited;

    const remaining = await caches.keys();
    expect(remaining).not.toContain("talyvor-static-v1"); // old version gone
    expect(remaining).not.toContain("talyvor-api-v1");
  });

  // Real-time surfaces must never be served from cache.
  it("skips collab / mcp / search so they never return a stale snapshot", async () => {
    const { handlers } = loadSW(caches, fetchMock as unknown as typeof fetch);
    expect(await runFetch(handlers, "/v1/collab/p1/ws", { mode: "cors" })).toBeNull();
    expect(await runFetch(handlers, "/v1/workspaces/ws1/search", { mode: "cors" })).toBeNull();
  });
});
