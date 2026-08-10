import { beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { matchRoutes } from "react-router-dom";

// "OPEN" ON A PENDING APPROVAL NAVIGATED TO A URL THIS APP HAS NO ROUTE FOR.
//
// ApprovalInbox.tsx:49 called `onOpenPage("", req.page_id)` with a HARDCODED empty spaceID,
// and routes.tsx:128 wires that prop to `navigate(paths.page(spaceID, pageID))`. So the
// button produced `/spaces//pages/{id}` — which the REAL route table resolves to `*`,
// i.e. NotFoundView. Measured with matchRoutes against the app's own `routes` (the pure
// resolver routes.test.tsx already uses — "a route that matches here is the route that
// mounts"), at 41367df:
//
//   /spaces/sp-1/pages/p-1   ->  spaces/:spaceID/pages/:pageID   (positive control)
//   /spaces//pages/p-1       ->  *                               (what the button sent)
//   /pages/p-1               ->  *                               (finding (12)(a), not this)
//
// ⚠ THE COMMENT BESIDE IT ASSERTED THE OPPOSITE, AND EXACTLY HALF OF IT WAS TRUE. It said
// (i) "the server's decide endpoint doesn't actually need the spaceID (it's URL-decorative)"
// and (ii) "the page route is reconstructed by PageView once the user navigates in".
// (i) IS TRUE AND WAS MEASURED, not assumed — through the shipped chain (gatewayauth → authz
// → a real permission.Enforcer) on real Postgres, `POST /v1/spaces//pages/{p}/approval/{r}/decide`
// answers 200 {"ok":true} and records the decision, exactly as the same request with the real
// space does (internal/approval/inbox_space_realpg_test.go). chi matches an empty {spaceID}
// segment, and the route's enforcer resolves the page from {pageID}. So the inline
// Approve/Reject buttons WORKED; only Open did not.
// (ii) IS FALSE: PageView never mounts, because the route never matches. There is nothing
// downstream to reconstruct anything.
//
// THE FIX IS DATA PLUMBING, NOT A DESIGN CALL: the pending row already carries page_id, and
// every page has a space (pages.space_id is `TEXT NOT NULL REFERENCES spaces(id)`,
// migrations/0002_pages.sql:13). ListPending now JOINs it and the row carries `space_id`.
//
// ⚠ THE ADDRESS BAR SHOWS `/spaces/pages/{id}`, NOT `/spaces//pages/{id}`, AND THAT MATTERS
// TO WHOEVER GREPS FOR IT NEXT. `paths.page("", id)` builds the double slash; navigate()
// normalises it away. Measured on the SHIPPED createBrowserRouter (App.tsx) in jsdom — built
// `/spaces//pages/p-1`, landed `/spaces/pages/p-1` in BOTH router.state.location.pathname and
// window.location.pathname, leaf `*`; the same navigation with a real space lands on the page
// route. Both spellings resolve to the catch-all, so the verdict is the same either way.
//
// WHAT THIS TEST ASSERTS, AND WHY IN THIS SHAPE: it drives the REAL route table from the
// REAL inbox component and reads the resolved leaf route, so it fails for the actual defect
// (a URL nothing serves) rather than for the shape of a string. Asserting the pathname alone
// would pass a hand-built literal that no route matches; asserting the space id comes from
// the API response is what stops a hardcoded constant from satisfying it.
//
// ⚠ THE ROW IS LOCATED BY THE PAGE ID IN ITS ACCESSIBLE NAME, NOT BY THE WORD "Page", AND
// THAT IS A CONTROL RESULT: with a copy-coupled selector, C6 (renaming the row's label) red
// all three tests. A navigation guard that reds on a copy edit is a guard someone deletes.
//
// 12/12 controls as predicted — scripts/w31-inbox-space-controls.py, verdict read as THE SET
// OF ASSERTIONS THAT FIRED. The pair that carries this file is C1/C2: the same line, one
// putting the empty string back (all three tests red) and one substituting a CONSTANT that is
// right for the first row (only the third test reds). Neither is worth anything alone — a
// guard reddening on any edit to that line passes C1 and cannot see C2, and a guard asserting
// only the URL's shape passes C2 while Open still lands in the wrong space. C11 (the request
// id where the page id belongs) is what earns the params assertion: the URL still RESOLVES,
// so the leaf assertion stays green and only the ids can speak.

const pending = vi.fn();
vi.mock("~/api/approval", () => ({
  approvalApi: {
    pending: (...a: unknown[]) => pending(...a),
    decide: vi.fn().mockResolvedValue({ ok: true }),
  },
}));

// The chrome and the heavy authoring surface are not the subject. Layout → Outlet keeps the
// sidebar's own query tree out; PageView → a marker that says WHICH page mounted, so "the
// route matched" and "the right page opened" are separate observations.
vi.mock("./Layout", async () => {
  const { Outlet } = await import("react-router-dom");
  return { Layout: () => <Outlet /> };
});
vi.mock("~/pages/PageView", () => ({
  PageViewPage: ({ pageID }: { pageID: string }) => <div>PAGE_RENDERED:{pageID}</div>,
}));
vi.mock("~/hooks/usePage", () => ({
  usePage: () => ({ isLoading: false, isError: false, error: null, data: { id: "pg-77", workspace_id: "ws-test" } }),
  usePages: () => ({ data: [], isLoading: false }),
  useUpdatePage: () => ({ mutate: vi.fn() }),
}));
vi.mock("~/hooks/useSpaces", () => ({
  useSpace: () => ({ isLoading: false, isError: false, error: null, data: { id: "sp-42", name: "Ops" } }),
  useSpaces: () => ({ data: [], isLoading: false }),
  useCreateSpace: () => ({ mutate: vi.fn() }),
  workspaceID: () => "ws-test",
}));

const ROW = {
  id: "req-1",
  page_id: "pg-77",
  space_id: "sp-42",
  workspace_id: "ws-test",
  requested_by: "mbr-author",
  reviewers: ["mbr-me"],
  message: "",
  status: "pending",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

// A QueryClient with retries off, because a retrying client turns a mistake in this file into
// a timeout instead of a failure. It is not the subject here — the navigation is.
async function renderInbox() {
  const { createMemoryRouter, RouterProvider } = await import("react-router-dom");
  const { routes } = await import("./routes");
  const router = createMemoryRouter(routes, { initialEntries: ["/approvals"] });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

// leafOf is the same resolver routes.test.tsx uses: the deepest matched route's path is the
// one whose element renders. `*` is NotFoundView.
async function leafOf(pathname: string) {
  const { routes } = await import("./routes");
  const m = matchRoutes(routes, pathname);
  return { leaf: m?.[m.length - 1]?.route.path, params: m?.[m.length - 1]?.params ?? {} };
}

beforeEach(() => {
  cleanup();
  pending.mockReset();
  pending.mockResolvedValue([ROW]);
  localStorage.setItem("docs_member_id", "mbr-me");
});

describe("the approval inbox's Open button", () => {
  it("navigates to a URL the app's own route table resolves to the page route", async () => {
    const router = await renderInbox();
    const open = await screen.findByRole("button", { name: /pg-77/ });

    // PRECONDITION: the instrument can tell the two answers apart. If `*` and the page route
    // were indistinguishable here, everything below would pass on any string at all.
    expect((await leafOf("/spaces/sp-1/pages/p-1")).leaf).toBe("spaces/:spaceID/pages/:pageID");
    expect((await leafOf("/spaces//pages/p-1")).leaf).toBe("*");

    await userEvent.click(open);

    const dest = router.state.location.pathname;
    const { leaf, params } = await leafOf(dest);
    expect(leaf, `Open navigated to ${dest}, which this app has no route for`).toBe(
      "spaces/:spaceID/pages/:pageID",
    );
    // The ids must be the row's own — a hardcoded constant would satisfy the leaf check.
    expect(params).toEqual({ spaceID: "sp-42", pageID: "pg-77" });
  });

  it("actually mounts the page, rather than Not found", async () => {
    const router = await renderInbox();
    await userEvent.click(await screen.findByRole("button", { name: /pg-77/ }));
    expect(router.state.location.pathname).toBe("/spaces/sp-42/pages/pg-77");
    expect(await screen.findByText("PAGE_RENDERED:pg-77")).toBeInTheDocument();
    expect(screen.queryByText("Not found")).not.toBeInTheDocument();
  });

  it("carries the space the API reported, not one derived from anything else", async () => {
    // A second row with a DIFFERENT space: a fix that read the space from a single global
    // (the first space, the current space, a constant) passes the test above and fails here.
    pending.mockResolvedValue([ROW, { ...ROW, id: "req-2", page_id: "pg-88", space_id: "sp-99" }]);
    const router = await renderInbox();
    await userEvent.click(await screen.findByRole("button", { name: /pg-88/ }));
    expect(router.state.location.pathname).toBe("/spaces/sp-99/pages/pg-88");
  });
});
