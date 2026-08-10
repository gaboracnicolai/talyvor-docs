import { beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { act } from "react";
import { useSearch } from "./useSearch";

// THE CACHE KEY OMITTED `offset` WHILE CARRYING EVERY OTHER OPTION.
//
//   queryKey: ["search", workspaceId, debounced, opts.type, opts.spaceId, opts.limit]
//   queryFn:  () => searchApi.search(workspaceId, debounced, opts)   ← opts, whole
//
// react-query caches on the KEY alone, so two searches differing only in `offset` are ONE cache
// entry: within staleTime the second is served the first one's rows and no request is made. Every
// sibling option is in the key, which is what makes this an omission rather than a decision.
//
// ⚠ STALETIME IS *NOT* WHAT MAKES THIS VISIBLE, AND I PREDICTED THE OPPOSITE. This file first
// imported the app's client options through a new `~/lib/queryClient` module, on the argument that
// the `{ retry: false }` stand-in every other hook test builds would be blind to a caching defect.
// Control F3 falsified that: with the key reverted AND staleTime set to 0, the guard STILL reds.
// react-query refetches on a MOUNT or a KEY CHANGE, not on a rerender — and the whole defect is
// that the key does not change, so no staleTime can rescue it. The module was reverted rather than
// shipped as unearned ceremony; the options below are the app's values (main.tsx) and they are
// documentation, not the mechanism.
//
// ⚠ IT IS LATENT TODAY AND SAYS SO: `SearchModal` calls `useSearch(workspaceId)` with no options,
// so nothing in the SPA passes an offset yet. This is a guard on the seam, not a repair of a
// visible screen — the same class the `offset` fix in internal/search closed one layer down.

vi.mock("~/api/search", () => ({
  searchApi: { search: vi.fn() },
}));
import { searchApi } from "~/api/search";

const mockSearch = searchApi.search as unknown as ReturnType<typeof vi.fn>;

// One client for the whole hook lifetime — a rerender with a new offset must reach the SAME cache,
// which is what a pagination control does.
function makeWrapper() {
  const qc = new QueryClient({
    // main.tsx's values. See the note above: measured NOT to be what makes the defect visible.
    defaultOptions: { queries: { staleTime: 30_000, retry: 1, refetchOnWindowFocus: false } },
  });
  return function W({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockSearch.mockImplementation((_ws: string, _q: string, opts: { offset?: number } = {}) =>
    Promise.resolve({
      results: [{ page_id: `p-${opts.offset ?? 0}`, page_title: "t", space_name: "s", headline: "", source: "both", url: "/u" }],
      total: 1,
      query: "vector",
      took_ms: 1,
    }),
  );
});

describe("useSearch cache key", () => {
  it("a different offset is a different request, not the previous page from cache", async () => {
    const W = makeWrapper();
    const { result, rerender } = renderHook(
      ({ offset }: { offset: number }) => useSearch("ws-1", { limit: 10, offset }),
      { wrapper: W, initialProps: { offset: 0 } },
    );

    act(() => result.current.setQuery("vector"));
    await waitFor(() => expect(mockSearch).toHaveBeenCalledTimes(1));
    // PREMISE: page 1 really was fetched and really did answer. Without this, an absent second
    // request below could mean the hook never ran at all.
    await waitFor(() => expect(result.current.data?.results[0].page_id).toBe("p-0"));

    rerender({ offset: 10 });

    // THE DEFECT. The key does not carry `offset`, so this is a cache HIT on page 1's entry:
    // no second call, and the rows are page 1's.
    await waitFor(
      () => expect(mockSearch).toHaveBeenCalledTimes(2),
      { timeout: 2000 },
    ).catch(() => {
      throw new Error(
        `offset MISSING FROM THE CACHE KEY: rerendering with offset=10 made ${mockSearch.mock.calls.length} ` +
          `request(s), want 2 — the second search was served page 1's cache entry. Calls: ` +
          JSON.stringify(mockSearch.mock.calls.map((c) => c[2])),
      );
    });
    expect(mockSearch.mock.calls.map((c) => (c[2] as { offset?: number }).offset)).toEqual([0, 10]);
    await waitFor(() => expect(result.current.data?.results[0].page_id).toBe("p-10"));
  });

  it("MUST STAY GREEN: an unchanged offset is still one request, not two", async () => {
    // The over-correction refusal. A key that changes on every render — or a fix that disables the
    // cache — satisfies the case above perfectly and destroys the reason the cache exists.
    const W = makeWrapper();
    const { result, rerender } = renderHook(
      ({ offset }: { offset: number }) => useSearch("ws-1", { limit: 10, offset }),
      { wrapper: W, initialProps: { offset: 0 } },
    );
    act(() => result.current.setQuery("vector"));
    await waitFor(() => expect(mockSearch).toHaveBeenCalledTimes(1));
    rerender({ offset: 0 });
    rerender({ offset: 0 });
    await new Promise((r) => setTimeout(r, 400));
    expect(mockSearch).toHaveBeenCalledTimes(1);
  });

  it("MUST STAY GREEN: the sibling options still key the cache", async () => {
    // `type`, `spaceId` and `limit` were already in the key. A fix that rewrites the key must not
    // drop one on the way past — and nothing else in this file would notice.
    const W = makeWrapper();
    const { result, rerender } = renderHook(
      ({ limit }: { limit: number }) => useSearch("ws-1", { limit }),
      { wrapper: W, initialProps: { limit: 10 } },
    );
    act(() => result.current.setQuery("vector"));
    await waitFor(() => expect(mockSearch).toHaveBeenCalledTimes(1));
    rerender({ limit: 25 });
    await waitFor(() => expect(mockSearch).toHaveBeenCalledTimes(2), { timeout: 2000 });
  });
});
