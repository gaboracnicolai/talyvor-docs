import { describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";

// In-memory history over the REAL route table. This is the honest slice of "Back/Forward
// works": react-router's memory history stack is what the browser's history maps onto, so
// proving navigate()/navigate(-1) move between the app's routes proves the route tree
// participates in history correctly. What this CANNOT prove — the physical Back button
// dispatching popstate, and the address bar — is browser chrome and is click-to-verify.

vi.mock("~/hooks/usePage", () => ({
  usePage: () => ({ isLoading: false, isError: false, error: null, data: { id: "p1", workspace_id: "ws" } }),
  usePages: () => ({ data: [], isLoading: false }),
  useUpdatePage: () => ({ mutate: vi.fn() }),
}));
vi.mock("~/hooks/useSpaces", () => ({
  useSpace: () => ({ isLoading: false, isError: false, error: null, data: { id: "s1", name: "S" } }),
  useSpaces: () => ({ data: [], isLoading: false }),
  useCreateSpace: () => ({ mutate: vi.fn() }),
  workspaceID: () => "ws",
}));
vi.mock("./Layout", async () => {
  const { Outlet } = await import("react-router-dom");
  return { Layout: () => <Outlet /> };
});
vi.mock("~/pages/Home", () => ({ HomePage: () => <div>HOME</div> }));
vi.mock("~/pages/SpaceView", () => ({ SpaceViewPage: () => <div>SPACE</div> }));
vi.mock("~/pages/PageView", () => ({ PageViewPage: () => <div>PAGE</div> }));

describe("router history over the real routes", () => {
  it("navigates forward and back through the history stack", async () => {
    const { createMemoryRouter, RouterProvider } = await import("react-router-dom");
    const { routes } = await import("./routes");
    const { paths } = await import("./paths");

    const router = createMemoryRouter(routes, { initialEntries: ["/"] });
    render(<RouterProvider router={router} />);
    expect(router.state.location.pathname).toBe("/");

    await router.navigate(paths.space("s1"));
    expect(router.state.location.pathname).toBe("/spaces/s1");

    await router.navigate(paths.page("s1", "p1"));
    expect(router.state.location.pathname).toBe("/spaces/s1/pages/p1");

    // Back — the Back-button equivalent at the history level.
    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/spaces/s1");

    // Back again → home.
    await router.navigate(-1);
    expect(router.state.location.pathname).toBe("/");

    // Forward.
    await router.navigate(1);
    expect(router.state.location.pathname).toBe("/spaces/s1");
  });

  it("lands directly on a deep URL (the deep-link case, in memory)", async () => {
    const { createMemoryRouter, RouterProvider } = await import("react-router-dom");
    const { routes } = await import("./routes");

    const router = createMemoryRouter(routes, { initialEntries: ["/spaces/s1/pages/p1"] });
    render(<RouterProvider router={router} />);
    // Starting at a deep entry resolves the nested route immediately — no pass through home.
    expect(router.state.location.pathname).toBe("/spaces/s1/pages/p1");
    expect(document.body.textContent).toContain("PAGE");
  });
});
