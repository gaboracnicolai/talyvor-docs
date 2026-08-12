import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { DatabaseBlock } from "./DatabaseBlock";

// A SAVED VIEW'S HIDDEN COLUMNS WERE STORED, SERVED, AND RENDERED ANYWAY.
//
// `database_views.hidden_cols` is a `TEXT[]` (migrations/0007_databases.sql:42) that the store
// selects on every read (`viewSelectCols`), inserts on create, and allow-lists on PATCH
// (internal/database/store.go). It reaches the SPA on the `views` payload and
// `frontend/src/api/database.ts:58` declares it. Then every renderer takes the WHOLE schema:
// DatabaseBlock passed `database.schema` to TableView / ListView / KanbanView / GalleryView and
// `hidden_cols` appeared in no component at all. The mechanism was maintained on both sides of the
// wire and connected on neither — the same shape as `filters`/`sort` in DatabaseBlock.view.test.tsx
// one merge earlier, in the same component, on the same view object.
//
// ⚠ MEASURED THROUGH THE SHIPPED ROUTES ON REAL POSTGRES BEFORE ANY CHANGE, not read. One
// database, schema `c-status` / `c-secret` / `c-title`, one row, through the production
// gatewayauth + authz + dbEnf chain:
//
//   POST  /v1/databases/{id}/views  {"hidden_cols":["c-secret"]}
//     -> 201 {…,"hidden_cols":["c-secret"],…}                     stored as sent
//   GET   /v1/databases/{id}/views
//     -> 200 [{…,"hidden_cols":["c-secret"],…}]                   THIS is what useDatabase receives
//   PATCH /v1/databases/{id}/views/{viewID} {"hidden_cols":["c-secret","c-status"]}
//     -> 200 {…,"hidden_cols":["c-secret","c-status"],…}          the second producer works too
//   GET   /v1/databases/{id}/rows?view_id={viewID}
//     -> 200 [{"values":{"c-secret":"PROBE-SALARY-482000","c-status":"todo","c-title":"alpha"}}]
//
// THE LAST LINE IS WHY THIS IS THE RENDERER'S JOB AND NOT THE SERVER'S: `ListRows` returns ROWS,
// not columns, so a hidden column's CELL is on the wire for every request. Nothing downstream of
// the fetch dropped it. The stub below answers exactly those payloads.
//
// ⚠ WHAT THIS IS NOT: a repair of a screen a user can currently break. No surface in this SPA
// writes `hidden_cols` — DatabaseBlock's `createView.mutate` sends `{database_id, name, type,
// sort_dir}` and nothing calls `updateView` with it — so every view the PRODUCT creates carries
// `[]` and the fix is a no-op for it. It is live for the API and MCP clients that can set it
// (measured above), and it is a guard on the seam either way. Stated plainly here because a guard
// sold as a user-visible fix is a guard whose value nobody can check.
//
// ⚠ AND THE HAZARD IN THE OBVIOUS FIX, WHICH [SCHEMA-INTACT] EXISTS FOR: filtering at the source
// (`const schema = (database.schema ?? []).filter(...)`) also feeds `addColumn`, which sends
// `[...schema, next]` to `PATCH /databases/{dbID}/schema` — a WHOLE-SCHEMA replace. Hiding a
// column would then DELETE it from the database the next time anyone added one. Hiding is a
// per-view render concern; the schema the write path sends must stay whole.

const HIDDEN_VALUE = "PROBE-SALARY-482000";
const VISIBLE_VALUE = "STILL-VISIBLE-TITLE";

const DB = {
  id: "db-1",
  page_id: "p-1",
  workspace_id: "w-1",
  name: "Tasks",
  schema: [
    { id: "c-status", name: "Status", type: "select", options: ["todo", "done"] },
    { id: "c-secret", name: "Salary", type: "text" },
    { id: "c-title", name: "Title", type: "text" },
  ],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

// The saved view as the measured server returns it: `c-secret` hidden, no filters, no sort.
const VIEW = {
  id: "view-1",
  database_id: "db-1",
  name: "Table",
  type: "table",
  filters: [],
  sort_by: "",
  sort_dir: "asc",
  group_by: "",
  hidden_cols: ["c-secret"],
  created_at: "2026-01-01T00:00:00Z",
};

// One saved view per tab, each hiding the same column. THE FOUR RENDERERS ARE FOUR SEPARATE
// PROP SITES and a control proved a guard on one says nothing about the others: reverting only
// `<ListView schema=…>` left the whole 22-file suite green (C1b), and the same for the gallery
// (C1c). [EVERY-TAB] below is what those two controls now fail against.
//
// ⚠ c-secret IS THE FIRST TEXT COLUMN ON PURPOSE. ListView takes `schema.find(text)` as the row
// title and GalleryView takes the first text/url/checkbox as the card headline, so an unfixed
// list or gallery does not merely leak the hidden column as a side field — it prints it as the
// PRIMARY label of every row.
const TAB_VIEWS = [
  VIEW,
  { ...VIEW, id: "view-list", name: "List", type: "list" },
  { ...VIEW, id: "view-kanban", name: "Kanban", type: "kanban" },
  { ...VIEW, id: "view-gallery", name: "Gallery", type: "gallery" },
];

// A view that hides the database's ONLY select column, with no grouping chosen yet. Opening the
// Kanban tab is the one place DatabaseBlock WRITES a column id back to the server, and this is
// the fixture where the column it would pick is one the view hides.
const HIDES_THE_SELECT = [
  { ...VIEW, hidden_cols: ["c-status"] },
  { ...VIEW, id: "view-kanban", name: "Kanban", type: "kanban", group_by: "", hidden_cols: ["c-status"] },
];

// The row as the measured server returns it: EVERY cell, including the hidden column's.
const ROW = {
  id: "r-1",
  database_id: "db-1",
  values: { "c-status": "todo", "c-secret": HIDDEN_VALUE, "c-title": VISIBLE_VALUE },
  position: 1,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

interface Sent {
  method: string;
  url: string;
  body: string;
}

function stubServer(views: unknown[] = [VIEW]) {
  const sent: Sent[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init?: RequestInit) => {
      sent.push({
        method: init?.method ?? "GET",
        url,
        body: typeof init?.body === "string" ? init.body : "",
      });
      let payload: unknown = {};
      if (url.includes("/views")) {
        payload = init?.method && init.method !== "GET" ? VIEW : views;
      } else if (url.includes("/rows")) {
        payload = [ROW];
      } else if (url.includes("/databases/db-1")) {
        payload = DB;
      }
      return new Response(JSON.stringify(payload), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      });
    }),
  );
  return sent;
}

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const W = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>{children}</QueryClientProvider>
  );
  return render(<DatabaseBlock databaseID="db-1" />, { wrapper: W });
}

// The header cell is an <input value={col.name}>, so a column NAME never reaches textContent.
// Reading the inputs is the only way to see it.
const inputValues = (container: HTMLElement) =>
  Array.from(container.querySelectorAll("input")).map((el) => (el as HTMLInputElement).value);

beforeEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("DatabaseBlock honours the active view's hidden columns", () => {
  // ⚠ ONE ASSERTION PER TEST, ON PURPOSE. A `waitFor` that throws ends the whole `it`, so two
  // assertions in one body mean a control that moves only the second one is scored against the
  // first — DatabaseBlock.view.test.tsx records that exact mistake being made and repaired.

  it("[HIDDEN-CELL] a hidden column's value is not painted", async () => {
    stubServer();
    const { container } = mount();

    // The row has to have landed before an absence means anything: an empty table trivially does
    // not contain the value. [VISIBLE] is the premise, asserted here as a precondition.
    await waitFor(() => expect(container.textContent ?? "").toContain(VISIBLE_VALUE));

    await waitFor(
      () =>
        expect(
          container.textContent ?? "",
          "[HIDDEN-CELL] the active view hides c-secret and its cell is on screen — the saved " +
            "view's hidden_cols reached this component and nothing applied them",
        ).not.toContain(HIDDEN_VALUE),
      { timeout: 2000 },
    );
  });

  it("[HIDDEN-HEADER] a hidden column's header is not rendered", async () => {
    stubServer();
    const { container } = mount();

    await waitFor(() => expect(inputValues(container)).toContain("Title"));

    await waitFor(
      () =>
        expect(
          inputValues(container),
          "[HIDDEN-HEADER] the hidden column still has a header — a column with no cells but a " +
            "live name/type editor is still a rendered column",
        ).not.toContain("Salary"),
      { timeout: 2000 },
    );
  });

  it("[VISIBLE] the columns the view does NOT hide are still rendered", async () => {
    // MUST STAY GREEN. Every assertion above is satisfied by rendering nothing at all; this is the
    // one that says the fix hides exactly the named columns and not the table.
    //
    // ⚠ NO CONTROL CLAIMS THIS ONE ALONE, AND THAT IS RECORDED RATHER THAN GLOSSED. The two
    // assertions above open by WAITING for this same column, because an absence asserted against a
    // screen that never rendered is green for the wrong reason. So any mutation that drops a
    // visible column fails their preconditions too, and there is no mutation only [VISIBLE] sees.
    // It is kept for legibility — it names the property, so the failure says which half broke —
    // not because a control earns it. C2 (filter inverted) fires it together with five others,
    // two of them in DatabaseBlock.view.test.tsx.
    stubServer();
    const { container } = mount();

    await waitFor(
      () =>
        expect(
          inputValues(container),
          "[VISIBLE] a column the view does not hide lost its header — the filter is hiding more " +
            "than hidden_cols names",
        ).toContain("Title"),
      { timeout: 2000 },
    );
  });

  it("[SCHEMA-INTACT] adding a column sends a schema that still contains the hidden one", async () => {
    // MUST STAY GREEN, AND IT IS THE ASSERTION THE OBVIOUS FIX FAILS. `PATCH /schema` replaces the
    // WHOLE schema. If the hidden column is filtered out where `schema` is defined, the next
    // "add column" writes a schema without it and the column — and every cell under it — is gone
    // from the database, for every view.
    const sent = stubServer();
    const { container, findByTitle } = mount();

    await waitFor(() => expect(inputValues(container)).toContain("Title"));
    (await findByTitle("Add column")).click();

    await waitFor(
      () => {
        const patch = sent.find((s) => s.method === "PATCH" && s.url.includes("/schema"));
        expect(patch, "[SCHEMA-INTACT] no schema PATCH was sent").toBeTruthy();
        expect(
          patch!.body,
          "[SCHEMA-INTACT] the schema written back does not contain the hidden column — hiding a " +
            `column deleted it. Body sent: ${patch!.body}`,
        ).toContain("c-secret");
      },
      { timeout: 2000 },
    );
  });

  it("[RENAME-INTACT] renaming a column sends a schema that still contains the hidden one", async () => {
    // MUST STAY GREEN, AND IT IS HERE BECAUSE A CONTROL FOUND THE HOLE. `addColumn` is not the
    // only whole-schema write: `renameColumn` and `retypeColumn` also `PATCH /schema` with a
    // rebuilt array. Control C6 pointed rename at the VISIBLE schema — the same destructive edit
    // [SCHEMA-INTACT] catches on the add path — and the whole 22-file suite stayed green. One
    // assertion per write path, because "the schema stays whole" is a claim about each of them
    // and [SCHEMA-INTACT] can only speak for the one it drives.
    const sent = stubServer();
    const { container } = mount();

    await waitFor(() => expect(inputValues(container)).toContain("Title"));
    const header = Array.from(container.querySelectorAll("input")).find(
      (el) => (el as HTMLInputElement).value === "Title",
    );
    expect(header, "[RENAME-INTACT] no header input for the visible column").toBeTruthy();
    fireEvent.change(header!, { target: { value: "Renamed" } });

    await waitFor(
      () => {
        const patch = sent.find((s) => s.method === "PATCH" && s.url.includes("/schema"));
        expect(patch, "[RENAME-INTACT] no schema PATCH was sent").toBeTruthy();
        expect(
          patch!.body,
          "[RENAME-INTACT] the schema written back does not contain the hidden column — renaming " +
            `a visible column deleted the hidden one. Body sent: ${patch!.body}`,
        ).toContain("c-secret");
      },
      { timeout: 2000 },
    );
  });

  it.each([
    ["List", "c-secret is this view's row title"],
    ["Kanban", "c-secret is a card field"],
    ["Gallery", "c-secret is this view's card headline"],
  ])("[EVERY-TAB] the %s tab hides it too (%s)", async (label) => {
    // The other three renderers. Each is its own `schema=` prop site, and controls C1b/C1c
    // measured that a guard on the table one is green while they are wrong.
    stubServer(TAB_VIEWS);
    const { container } = mount();

    await waitFor(() => expect(container.textContent ?? "").toContain(VISIBLE_VALUE));
    const tab = Array.from(container.querySelectorAll("button")).find(
      (b) => (b.textContent ?? "").trim() === label,
    );
    expect(tab, `[EVERY-TAB] no ${label} tab button`).toBeTruthy();
    fireEvent.click(tab!);

    // The row must be on screen in THIS tab before its absence means anything — otherwise an
    // empty tab passes for a tab that hides the right column.
    await waitFor(() => expect(container.textContent ?? "").toContain(VISIBLE_VALUE));
    await waitFor(
      () =>
        expect(
          container.textContent ?? "",
          `[EVERY-TAB] the ${label} view painted the hidden column — hidden_cols is applied in ` +
            "the table renderer and not this one",
        ).not.toContain(HIDDEN_VALUE),
      { timeout: 2000 },
    );
  });

  it("[NO-HIDDEN-GROUP] opening Kanban does not save a hidden column as the grouping", async () => {
    // THE ONE PLACE THIS COMPONENT WRITES A COLUMN ID BACK. Clicking Kanban with no grouping set
    // PATCHes the view with `group_by: <first select column>`. Resolved over the whole schema,
    // that is a column THIS VIEW HIDES — persisted, server-side, as the view's grouping. Resolved
    // over the visible schema there is no candidate and no write happens.
    //
    // ⚠ THIS PINS THE WIRE, NOT THE PIXELS, DELIBERATELY. What a kanban SHOULD render when its
    // grouping column is hidden is a product question this merge does not answer (both readings
    // land on the same "choose a select column" prompt today, because KanbanView looks the group
    // column up in the schema it was handed). What is not a product question is whether the SPA
    // may quietly store a hidden column as a view's grouping. Control C5 is exactly this line and
    // was NOT CAUGHT by anything until this assertion existed.
    const sent = stubServer(HIDES_THE_SELECT);
    const { container } = mount();

    await waitFor(() => expect(container.textContent ?? "").toContain(VISIBLE_VALUE));
    const tab = Array.from(container.querySelectorAll("button")).find(
      (b) => (b.textContent ?? "").trim() === "Kanban",
    );
    expect(tab, "[NO-HIDDEN-GROUP] no Kanban tab button").toBeTruthy();
    fireEvent.click(tab!);

    // The click has to have LANDED before its absence of a side effect means anything. The kanban
    // surface with no resolved grouping column is this prompt — and it is the same prompt with and
    // without the fix (KanbanView looks the group column up in the schema it was handed, which
    // does not contain the hidden one either way), so this precondition is blind to the property
    // under test and cannot smuggle the assertion.
    await waitFor(() =>
      expect(container.textContent ?? "").toContain("Choose a select column"),
    );
    // Then a tick, so any request the effects were going to make has been made: an absence
    // asserted inside the latency is an absence that proves nothing.
    await new Promise((r) => setTimeout(r, 50));

    const groupWrites = sent.filter(
      (s) => s.method === "PATCH" && s.url.includes("/views/") && s.body.includes("c-status"),
    );
    expect(
      groupWrites,
      "[NO-HIDDEN-GROUP] the component saved a column the view hides as its grouping: " +
        JSON.stringify(groupWrites),
    ).toEqual([]);
  });
});
