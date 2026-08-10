import { beforeEach, describe, expect, it, vi } from "vitest";

// THE OFFLINE READ PATH ANSWERED THE WRONG RESOURCE, SUCCESSFULLY.
//
// When fetch REJECTS, apiRequest hands the path to readCached(). Its page branch was
//
//   path.match(/\/v1\/spaces\/([^/]+)\/pages\/([^/?]+)/)
//
// which is anchored at neither end. `/v1/spaces/s1/pages/p1/comments` matches it — pageID
// captures "p1" and the trailing "/comments" is simply ignored — so the CACHED PAGE OBJECT is
// returned to a caller that asked for Comment[].
//
// ⚠ THE WRITE SIDE WAS ALREADY RIGHT, AND THAT IS THE POINT RATHER THAN A REPRIEVE. shouldCache
// gates writeCache with `/\/v1\/spaces\/[^/]+\/pages\/[^/]+$/` — ANCHORED. So the cache STORED
// only pages and SERVED a page for every sub-resource under one. Two hand-written patterns for
// one question, differing by a single `$`, and only the read half was wrong. The fix is the
// SHARED DEFINITION, not the character: both halves are now joins of one ordered list of
// cacheable path shapes, so they cannot drift apart again. (writeCache's own inner regex was
// unanchored too, but unreachable behind shouldCache — measured, not assumed: the write-side
// case below PASSED red-first, which is why this paragraph replaced the claim that it failed.)
//
// ⚠ MEASURED ON THE SHIPPED CLIENT BEFORE ANY CHANGE, by stubbing fetch to reject and calling
// apiRequest for every GET path this SPA actually builds (grepped from src/api/*.ts):
//
//   /v1/spaces/s1/pages/p1                      -> the cached Page   (correct)
//   /v1/spaces/s1/pages                         -> cached Page[]     (correct)
//   /v1/spaces                                  -> cached Space[]    (correct)
//   /v1/spaces/s1/pages/p1/comments             -> THE CACHED PAGE   (expects Comment[])
//   /v1/spaces/s1/pages/p1/comments/stats       -> THE CACHED PAGE   (expects a stats object)
//   /v1/spaces/s1/pages/p1/analytics?days=30    -> THE CACHED PAGE   (expects analytics)
//   /v1/spaces/s1/pages/p1/permissions          -> THE CACHED PAGE   (expects Permission[])
//   /v1/spaces/s1/pages/p1/approval             -> THE CACHED PAGE   (expects an approval req)
//   /v1/spaces/s1/pages/p1/changelog/entries    -> THE CACHED PAGE   (expects entries)
//   /v1/spaces/s1/pages/p1/edit-session         -> THE CACHED PAGE   (expects a session|null)
//   /v1/spaces/s1/permissions                   -> APIError "offline" (correct — no /pages/)
//
// SEVEN OF EIGHT PAGE-SCOPED GETS. The last row is the positive control: it proves the probe
// reached the real routing and that a correct cache MISS looks different from a hit.
//
// ⚠ THE FAILURE IS A SUCCESS, WHICH IS WHY NOTHING CAUGHT IT. The whole offline story in this
// client hangs off fetch REJECTING — that is the branch that consults the cache and the branch
// that queues writes. Here the reject IS taken and the recovery then RESOLVES with a value, so
// every caller sees a fulfilled promise carrying an object of the wrong shape. `.map` on a Page
// throws inside a component; a caller that reads a field it does not have gets undefined and
// renders an empty state. Neither reads as "you are offline".
//
// ⚠ AND IT IS NOT A TYPE ERROR ANYWHERE. readCached returns `T | null` via
// `page as unknown as T`, so the cast is the whole safety story and tsc cannot see through it.

const SENTINEL = { id: "p1", title: "THE PAGE OBJECT", space_id: "s1" };

vi.mock("~/lib/offlinedb", () => {
  const page = { id: "p1", title: "THE PAGE OBJECT", space_id: "s1" };
  return {
    cachePage: vi.fn().mockResolvedValue(undefined),
    cacheSpace: vi.fn().mockResolvedValue(undefined),
    getCachedPage: vi.fn().mockResolvedValue(page),
    getCachedPages: vi.fn().mockResolvedValue([page]),
    getCachedSpaces: vi.fn().mockResolvedValue([{ id: "s1", name: "Space" }]),
    queueWrite: vi.fn().mockResolvedValue(undefined),
  };
});

import { apiRequest, APIError } from "./client";
import { cachePage, getCachedPage } from "~/lib/offlinedb";

// Every page-scoped GET sub-resource this SPA builds, grepped from src/api/*.ts. Listed as
// LITERALS rather than derived, so a new sub-resource is a deliberate addition here and the
// list cannot silently shrink to whatever still passes.
const SUB_RESOURCES = [
  "/v1/spaces/s1/pages/p1/comments",
  "/v1/spaces/s1/pages/p1/comments/stats",
  "/v1/spaces/s1/pages/p1/analytics?days=30",
  "/v1/spaces/s1/pages/p1/permissions",
  "/v1/spaces/s1/pages/p1/approval",
  "/v1/spaces/s1/pages/p1/changelog/entries",
  "/v1/spaces/s1/pages/p1/edit-session",
];

function offline() {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
}

function online(body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => body,
      text: async () => JSON.stringify(body),
    }),
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  localStorage.setItem("docs_api_key", "k");
});

describe("the offline cache answers the resource that was asked for, or nothing", () => {
  for (const path of SUB_RESOURCES) {
    it(`offline GET ${path} does not resolve with the cached page`, async () => {
      offline();
      let resolved: unknown = Symbol("never");
      let threw: unknown = null;
      try {
        resolved = await apiRequest<unknown>(path);
      } catch (e) {
        threw = e;
      }

      // The precise defect: a FULFILLED promise carrying the wrong resource. Asserted before
      // the error shape, because "it threw the wrong error" and "it silently returned a Page"
      // are different failures and only one of them is this one.
      expect(
        resolved,
        `GET ${path} resolved with the cached PAGE object. The caller asked for a ` +
          `sub-resource and got a document. This is a fulfilled promise, so no offline ` +
          `handler anywhere in this app can see it.`,
      ).not.toEqual(SENTINEL);

      expect(threw, `GET ${path} should report being offline, not answer`).toBeInstanceOf(APIError);
      expect((threw as APIError).code).toBe("OFFLINE");
    });
  }

  // ONE-DIRECTIONAL AND DELIBERATE. Without these, "return null from readCached always" — i.e.
  // deleting the cache — satisfies every assertion above, and the offline feature would be
  // repaired by removing it.
  it("the page itself is still served from the cache", async () => {
    offline();
    await expect(apiRequest<unknown>("/v1/spaces/s1/pages/p1")).resolves.toEqual(SENTINEL);
  });

  it("the page itself is still served when the path carries a query string", async () => {
    // The anchoring must end the path at the pageID OR at the query, not at the pageID alone.
    offline();
    await expect(apiRequest<unknown>("/v1/spaces/s1/pages/p1?x=1")).resolves.toEqual(SENTINEL);
  });

  it("the page list is still served from the cache", async () => {
    offline();
    await expect(apiRequest<unknown>("/v1/spaces/s1/pages")).resolves.toEqual([SENTINEL]);
  });

  it("the space list is still served from the cache", async () => {
    offline();
    await expect(apiRequest<unknown>("/v1/spaces")).resolves.toEqual([{ id: "s1", name: "Space" }]);
  });

  it("a path with no /pages/ segment was always a miss and still is", async () => {
    // The measurement's positive control, kept as a guard: it is what distinguishes "the cache
    // stopped answering" from "the cache stopped answering the WRONG thing".
    offline();
    await expect(apiRequest<unknown>("/v1/spaces/s1/permissions")).rejects.toThrow("offline");
  });
});

describe("the cache stores the resource that was fetched, or nothing", () => {
  it("a sub-resource response is never written into the pages store", async () => {
    // The same unanchored regex on the way IN. A comments array has no `id`, so IndexedDB's
    // keyPath rejects it and the throw is swallowed by writeCache's catch — but an approval
    // request or an edit session DOES carry an `id`, and that is a non-page written into the
    // `pages` object store under it, where getCachedPage will hand it back as a Page.
    online({ id: "approval-1", status: "pending" });
    await apiRequest<unknown>("/v1/spaces/s1/pages/p1/approval");
    expect(
      vi.mocked(cachePage),
      "an approval response was written into the pages cache. getCachedPage would then " +
        "return it as a Page for anything holding that id.",
    ).not.toHaveBeenCalled();
  });

  it("the page response IS still written into the pages store", async () => {
    // One-directional companion to the case above: without it, "never cache anything" passes.
    online(SENTINEL);
    await apiRequest<unknown>("/v1/spaces/s1/pages/p1");
    expect(vi.mocked(cachePage)).toHaveBeenCalledWith(SENTINEL);
  });

  it("the cache is consulted at most once per offline read", async () => {
    // Guards the shape of the fix rather than its outcome: a repair that tries each branch in
    // turn until one answers would read the store repeatedly and could still fall through to
    // the page LIST branch for a sub-resource. This pins that the page path is decided once.
    offline();
    await apiRequest<unknown>("/v1/spaces/s1/pages/p1").catch(() => {});
    expect(vi.mocked(getCachedPage).mock.calls.length).toBeLessThanOrEqual(1);
  });
});
