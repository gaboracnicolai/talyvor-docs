import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { DatabaseBlock } from "./DatabaseBlock";

// THE SAVED VIEW'S FILTERS AND SORT WERE APPLIED BY THE SERVER AND NEVER ASKED FOR.
//
// `GET /v1/databases/{dbID}/rows` takes an optional `?view_id=`; when it is present the store
// resolves that saved view and applies its filters + sort (internal/database/store.go ListRows →
// filterRows / sortRows, ~130 lines and a five-operator matrix). DatabaseBlock resolves the active
// view, writes `sort_dir` into every view it creates, and says in its own comment that it picks a
// view "so the server can apply filters + sort" — then calls `useDatabase(databaseID)` with ONE
// argument, so the rows request never names it. The mechanism was maintained on both sides of the
// wire and connected on neither.
//
// ⚠ MEASURED ON BOTH SIDES BEFORE THE FIX, NOT READ:
//
//   the shipped component (this file, unfixed): GET /v1/databases/db-1
//                                               GET /v1/databases/db-1/views
//                                               GET /v1/databases/db-1/rows      ← no view_id, 0 of 1
//
//   the shipped server, real Postgres, through the production gatewayauth+authz+dbEnf chain,
//   one database with three rows (A status=done, B status=todo, C values {}) and a saved view
//   `status neq done, sort c-title desc`:
//
//     GET /rows                 -> A, B, <no c-title cell>      ← what the SPA asks for
//     GET /rows?view_id=V       -> B                            ← filtered and sorted
//     GET /rows?view_id=V (eq)  -> A                            ← the engine works both ways
//
// So the server half is correct and complete; the caller is what was missing. THE STUB BELOW
// REPRODUCES EXACTLY THOSE TWO ANSWERS — the unfiltered listing for a request with no `view_id`,
// the view's answer for one that has it — so what this file asserts about the screen is what the
// measured server would actually have sent.
//
// ⚠ WHAT THIS IS NOT: a repair of a visible screen. No surface in the SPA can build a filter today
// (`filters` appears in `frontend/src/api/database.ts` as a type and in no component), so every
// view the product creates carries empty filters and an empty `sort_by`, and `ListRows` no-ops on
// both. This is a guard on the seam, like the `offset` cache-key guard in useSearch.offset.test.tsx
// one layer down. It is stated here because a guard sold as a user-visible fix is a guard whose
// value nobody can check.
//
// ⚠ THE CACHE KEY IS PART OF THE FIX, AND A CONTROL PROVED IT AFTER I PREDICTED THE OPPOSITE.
// Control E4 (~/talyvor-queue/w31-dbview-controls.py) removes `activeView?.id` from the rows
// queryKey and leaves it in the queryFn; I predicted NOT CAUGHT — "one mount with one view cannot
// see a key collision" — and [VIEW-NAMED] and [SCREEN] both fired, logging the rows request as
// `GET /v1/databases/db-1/rows`. The views query resolves on a LATER tick than the first rows
// fetch, so the key changing is the only thing that makes react-query ask a second time. 10 of 11
// controls landed as predicted; this is the one that did not, and it made the guard stronger than
// the claim I had written for it.
//
// ⚠ AND ONE THING THE SEAM CARRIES THAT THIS FIX DOES NOT CLOSE, MEASURED IN THE SAME RUN: with
// `neq`, the row whose cell is ABSENT is dropped — applyFilter returns false for a missing cell
// before it ever reaches the operator, so `eq done` and `neq done` are BOTH false for the same
// row and the SPA's "+ New" button (which posts `values: {}`) produces exactly that row. Filed as
// measured-not-fixed rather than folded in here: it is a semantic call about what "is not X" means
// for an empty cell, and it is a different defect in a different file.

const DB = {
  id: "db-1",
  page_id: "p-1",
  workspace_id: "w-1",
  name: "Tasks",
  schema: [
    { id: "c-status", name: "Status", type: "select", options: ["todo", "done"] },
    { id: "c-title", name: "Title", type: "text" },
  ],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

// The saved view the server would filter by. `status neq done` excludes ROW_DONE.
const VIEW = {
  id: "view-1",
  database_id: "db-1",
  name: "Table",
  type: "table",
  filters: [{ col_id: "c-status", operator: "neq", value: "done" }],
  sort_by: "",
  sort_dir: "asc",
  group_by: "",
  hidden_cols: [],
  created_at: "2026-01-01T00:00:00Z",
};

const ROW_DONE = {
  id: "r-done",
  database_id: "db-1",
  values: { "c-status": "done", "c-title": "SHIPPED-ALREADY" },
  position: 1,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};
const ROW_OPEN = {
  id: "r-open",
  database_id: "db-1",
  values: { "c-status": "todo", "c-title": "STILL-OPEN" },
  position: 2,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

// stubServer answers like the measured server: the view's rows when the request names it, every
// row when it does not. `views` is what the second argument controls — [] is a database whose
// views have not been created yet, the first-load path.
function stubServer(views: unknown[]) {
  const urls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      urls.push(`${init?.method ?? "GET"} ${url}`);
      let payload: unknown = {};
      if (url.includes("/views")) {
        payload = views;
      } else if (url.includes("/rows")) {
        payload = url.includes(`view_id=${VIEW.id}`) ? [ROW_OPEN] : [ROW_DONE, ROW_OPEN];
      } else if (url.includes("/databases/db-1")) {
        payload = DB;
      }
      return new Response(JSON.stringify(payload), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return urls;
}

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const W = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return render(<DatabaseBlock databaseID="db-1" />, { wrapper: W });
}

const rowRequests = (urls: string[]) => urls.filter((u) => u.includes("/rows"));

beforeEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("DatabaseBlock names the view it renders", () => {
  // ⚠ THE THREE ASSERTIONS ARE THREE TESTS ON PURPOSE. The first version put [VIEW-NAMED] and
  // [SCREEN] in one `it`, and [VIEW-NAMED]'s waitFor threw before [SCREEN] ever ran — so a control
  // that moved only the screen would have been scored against the wire assertion. One assertion
  // per test is what makes the control verdicts readable.

  it("[VIEW-NAMED] asks for the active view's rows", async () => {
    const urls = stubServer([VIEW]);
    mount();

    // The view has to have loaded before the rows request can name it; without this the assertion
    // below could fail on a component that simply had not resolved a view yet.
    await waitFor(() => expect(urls.some((u) => u.includes("/views"))).toBe(true));
    await waitFor(() => expect(rowRequests(urls).length).toBeGreaterThan(0));

    await waitFor(
      () => {
        const named = rowRequests(urls).filter((u) => u.includes(`view_id=${VIEW.id}`));
        expect(
          named.length,
          `[VIEW-NAMED] no rows request named the active view. Rows requests seen: ${JSON.stringify(
            rowRequests(urls),
          )} — the saved view's filters and sort are applied by the server and this component never asks for them`,
        ).toBeGreaterThan(0);
      },
      { timeout: 2000 },
    );
  });

  it("[SCREEN] paints only the rows that view returns", async () => {
    // The user-visible half. The view excludes the done row, so the measured server never sends
    // it; a screen that shows it is a screen built from a different question than the one the
    // saved view asks. Separate from [VIEW-NAMED] because a request that names the view and a
    // screen that shows the filtered set are two claims, and only the second one is what a person
    // sees.
    stubServer([VIEW]);
    const { container } = mount();

    await waitFor(() => expect(container.textContent ?? "").toContain(ROW_OPEN.values["c-title"]));
    await waitFor(
      () =>
        expect(
          container.textContent ?? "",
          "[SCREEN] a row the active view filters out is on screen — the rows the component painted are not the rows the view returns",
        ).not.toContain(ROW_DONE.values["c-title"]),
      { timeout: 2000 },
    );
  });

  it("[FIRST-LOAD] a database whose views do not exist yet still lists its rows", async () => {
    // MUST STAY GREEN. The two assertions above are satisfied by gating the rows query on a view
    // id; that would leave a brand-new database — which has no views until DatabaseBlock's effect
    // creates one — with an empty table and no request at all. This is the assertion that catches
    // that, and neither of the two above can.
    const urls = stubServer([]);
    const { container } = mount();

    await waitFor(
      () => {
        expect(
          rowRequests(urls).length,
          "[FIRST-LOAD] no rows request was made for a database with no saved views — the rows query is gated on something that does not exist yet",
        ).toBeGreaterThan(0);
      },
      { timeout: 2000 },
    );
    await waitFor(() =>
      expect(
        container.textContent ?? "",
        "[FIRST-LOAD] the rows came back and the table is empty",
      ).toContain(ROW_OPEN.values["c-title"]),
    );
  });
});
