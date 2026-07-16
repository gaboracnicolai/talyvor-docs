import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { APIError } from "~/api/client";
import { NotFoundView, OfflineView } from "./views";

// Render-level tests. Two things the pure logic can't assert on its own:
//   (1) the not-found COPY leaks nothing — since 403 and 404 collapse to this one view, its
//       wording must not distinguish "forbidden" from "missing";
//   (2) the resource wrappers actually WIRE the guard — a 403/404 page query must render
//       NotFound, and success must render the page. Proven against the real route wrappers
//       with only the heavy chrome/editor stubbed out.

beforeEach(() => cleanup());

describe("NotFoundView copy — no existence oracle", () => {
  it("uses generic wording, never revealing that a resource exists-but-is-forbidden", () => {
    render(
      <MemoryRouter>
        <NotFoundView />
      </MemoryRouter>,
    );
    const text = document.body.textContent!.toLowerCase();
    expect(text).toContain("not found");
    // The moment any of these appear, a caller rotating ids learns which ones are real.
    for (const leak of ["forbidden", "access denied", "permission", "not authorized", "unauthorized", "403"]) {
      expect(text).not.toContain(leak);
    }
  });

  it("offline is a distinct, non-existence message", () => {
    render(
      <MemoryRouter>
        <OfflineView />
      </MemoryRouter>,
    );
    expect(document.body.textContent!.toLowerCase()).toContain("offline");
  });
});

// ── The wiring proof. Stub the chrome (Layout) and the heavy leaf (PageView) so this test
//    exercises routes.tsx's PageRoute + its guard, not the ProseMirror editor. The page/space
//    query hooks are mocked so we can drive each state deterministically.

const mockPage = vi.fn();
const mockSpace = vi.fn();

vi.mock("~/hooks/usePage", () => ({
  usePage: () => mockPage(),
  usePages: () => ({ data: [], isLoading: false }),
  useUpdatePage: () => ({ mutate: vi.fn() }),
}));
vi.mock("~/hooks/useSpaces", () => ({
  useSpace: () => mockSpace(),
  useSpaces: () => ({ data: [], isLoading: false }),
  useCreateSpace: () => ({ mutate: vi.fn() }),
  workspaceID: () => "ws-test",
}));
// Layout → just an Outlet, so we don't drag in the sidebar's own query tree.
vi.mock("./Layout", async () => {
  const { Outlet } = await import("react-router-dom");
  return { Layout: () => <Outlet /> };
});
// The heavy authoring surface → a marker. If the guard lets a forbidden page through, this
// marker appears and the test fails.
vi.mock("~/pages/PageView", () => ({
  PageViewPage: ({ pageID }: { pageID: string }) => <div>PAGE_RENDERED:{pageID}</div>,
}));

async function renderAt(pathname: string) {
  const { createMemoryRouter, RouterProvider } = await import("react-router-dom");
  const { routes } = await import("./routes");
  const router = createMemoryRouter(routes, { initialEntries: [pathname] });
  render(<RouterProvider router={router} />);
  return router;
}

describe("PageRoute wires the guard", () => {
  const ok = { isLoading: false, isError: false, error: null, data: { id: "p1", workspace_id: "ws-test" } };
  const loading = { isLoading: true, isError: false, error: null, data: undefined };

  beforeEach(() => {
    mockPage.mockReset();
    mockSpace.mockReset();
    mockSpace.mockReturnValue({ isLoading: false, isError: false, error: null, data: { id: "s1", name: "S" } });
  });

  it("renders the page when the page query succeeds", async () => {
    mockPage.mockReturnValue(ok);
    await renderAt("/spaces/s1/pages/p1");
    expect(await screen.findByText("PAGE_RENDERED:p1")).toBeInTheDocument();
  });

  it("renders NotFound (never the page) when the page query 404s", async () => {
    mockPage.mockReturnValue({ isLoading: false, isError: true, error: new APIError("nf", 404), data: undefined });
    await renderAt("/spaces/s1/pages/p1");
    expect(await screen.findByText("Not found")).toBeInTheDocument();
    expect(screen.queryByText("PAGE_RENDERED:p1")).not.toBeInTheDocument();
  });

  // THE SECURITY ASSERTION at the render level: a 403 must be indistinguishable from a 404.
  it("renders the IDENTICAL NotFound when the page query 403s — no oracle", async () => {
    mockPage.mockReturnValue({ isLoading: false, isError: true, error: new APIError("forbidden", 403), data: undefined });
    const { container } = render(<div />); // anchor to diff against
    void container;
    await renderAt("/spaces/s1/pages/p1");
    const forbidden = document.body.textContent;
    cleanup();

    mockPage.mockReturnValue({ isLoading: false, isError: true, error: new APIError("nf", 404), data: undefined });
    await renderAt("/spaces/s1/pages/p1");
    const missing = document.body.textContent;

    expect(forbidden).toContain("Not found");
    expect(forbidden).toBe(missing); // byte-identical: the router cannot tell them apart
  });

  it("shows loading while the page query is in flight", async () => {
    mockPage.mockReturnValue(loading);
    await renderAt("/spaces/s1/pages/p1");
    expect(await screen.findByText("Loading…")).toBeInTheDocument();
  });
});
