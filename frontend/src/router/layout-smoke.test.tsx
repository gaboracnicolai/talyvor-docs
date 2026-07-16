import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryRouter, RouterProvider } from "react-router-dom";
import { routes } from "./routes";

// Smoke test for the REAL chrome. The other tests stub Layout to isolate routing/guards; this
// one does the opposite — it mounts the actual Layout (real Sidebar/Header/Cmd-K search) under
// the router to catch a mount-time crash (bad import, hook misuse, null deref) that a mocked
// Layout would hide. Data queries fail here (fetch is stubbed to reject, no backend), which is
// the point: the chrome must render its loading/empty states, not throw.

const realFetch = globalThis.fetch;
beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("no backend in test")));
});
afterEach(() => {
  cleanup();
  globalThis.fetch = realFetch;
  vi.restoreAllMocks();
});

function renderApp(pathname: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createMemoryRouter(routes, { initialEntries: [pathname] });
  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("real Layout mounts under the router", () => {
  it("renders the home route inside the chrome without throwing, even with no backend", async () => {
    expect(() => renderApp("/")).not.toThrow();
    // The real chrome renders its stable structure regardless of (failed) data: the sidebar
    // shows its nav, the home view its "Spaces" section. Assert chrome text is present without
    // pinning a single node — the point is "mounted + rendered", not an exact tree.
    const spacesNodes = await screen.findAllByText(/spaces/i);
    expect(spacesNodes.length).toBeGreaterThan(0);
  });

  it("mounts a deep page URL directly (chrome + the page route) without throwing", () => {
    // We don't assert page CONTENT here (needs a backend); we assert the app doesn't crash
    // when it lands straight on a nested route — the deep-link mount path.
    expect(() => renderApp("/spaces/s1/pages/p1")).not.toThrow();
  });

  it("mounts the public shared route with NO chrome and does not throw", () => {
    expect(() => renderApp("/s/tok-1")).not.toThrow();
    // The shared viewer is outside Layout, so the sidebar chrome must NOT be present.
    expect(screen.queryByText(/needs review/i)).not.toBeInTheDocument();
  });
});
