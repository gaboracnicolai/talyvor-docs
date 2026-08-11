import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { SearchResult } from "~/api/search";

// THE SEARCH LIST PRINTED ONE ADDEND, AND THE DOCUMENT THAT ACTUALLY COST MONEY READ AS FREE.
//
// A page carries two independent costs and a derived sum (migration 0018):
//
//   ai_cost_usd        the cost of the Track ISSUES linked to this page
//   own_ai_cost_usd    the cost of AI operations performed ON this document
//   total_ai_cost_usd  their sum, derived on read
//
// `8b3e1be` (#54) made the search API emit all three; `b4f0b77` (#55) did the MCP surface;
// `198636a` (#56) did the page screen. THIS IS THE LAST COPY OF THAT SEAM — the search
// renderer — and it is the only one where two rows sit SIDE BY SIDE in one list, so getting
// it wrong is not an absolute error a reader might shrug at, it is a WRONG ORDERING between
// two documents on screen at once.
//
// ⚠ MEASURED ON THE SHIPPED COMPONENT BEFORE ANY CHANGE, by rendering it and reading the
// row's textContent — not by reading the JSX:
//
//   A own-funded   own 12.34 track 0    total 12.34  ->  "Ops·Runbooka runbook"       NO BADGE
//   B track-funded own 0     track 1.5  total 1.5    ->  "Ops·Runbook$1.50a runbook"
//   C free         own 0     track 0    total 0      ->  "Ops·Runbooka runbook"       no badge
//   D both halves  own 12.34 track 1.5  total 13.84  ->  "Ops·Runbook$1.50a runbook"  OFF BY 12.34
//   E semantic-only  all three ABSENT                ->  "Ops·Runbook"                no badge
//
// A AND C ARE BYTE-IDENTICAL IN THE COST REGION. The $12.34 document and the free one render
// the same, and D under-reports by 89% while showing the SMALLER half. `SearchModal.tsx`
// gated on `r.ai_cost_usd && r.ai_cost_usd > 0` — the TRACK half — and printed that half.
//
// ⚠ THE DATA WAS ALREADY ON THE WIRE. `internal/search/handler.go`'s Result carries all three
// as *float64 (`8b3e1be`), and `merge()` fills all three from the pages row for every
// full-text hit. `frontend/src/api/search.ts` declared `ai_cost_usd?` and NEITHER of the
// other two: they arrived and were dropped at the TYPE boundary, which is why nothing was red.
//
// ⚠ THE THIRD STATE, DECIDED AND SAID OUT LOUD RATHER THAN INHERITED. A semantic-only row is
// built from a vector hit with NO pages row read (handler.go merge(), second loop), so all
// three costs are nil and absent from the JSON. That is "NOT REPORTED", and it is a different
// fact from "measured and zero" — the whole reason `8b3e1be` made these pointers. The queue
// left the render of that state open: show nothing, show a marker, or pay a page read per row.
// PAYING A PAGE READ IS NOT DECIDED HERE — it is W3.1 finding (3) and it is a cost decision.
// CHOSEN: an em-dash marker, distinct from both a number and from silence. "Show nothing"
// was rejected because it is the status quo measured above: it makes state E identical to
// state C, so an unmeasured row reads as a free one, on a money surface, in a list where a
// priced row sits next to it. Under EITHER answer to finding (3) this renders the truth.

let rows: SearchResult[] = [];
// setQuery MUST be stable across renders: SearchModal's open-effect lists it in its deps, so a
// fresh closure per render is an infinite render loop. Measured the hard way — the first
// version of this mock spun a worker to 3.8GB RSS before it was killed.
const stableSetQuery = () => {};
vi.mock("~/hooks/useSearch", async () => {
  const actual = await vi.importActual<typeof import("~/hooks/useSearch")>("~/hooks/useSearch");
  return {
    ...actual,
    useSearch: () => ({
      query: "runbook",
      setQuery: stableSetQuery,
      debounced: "runbook",
      data: { results: rows, total: rows.length, query: "runbook", took_ms: 1 },
      isLoading: false,
      error: null,
    }),
  };
});

import { SearchModal } from "./SearchModal";

function renderModal() {
  return render(
    <SearchModal workspaceId="w1" open onClose={() => {}} onOpenPage={() => {}} />,
  );
}

// row builds a FULL-TEXT hit — the shape merge() produces when a pages row WAS read, so all
// three costs are known, including when they are zero.
function row(own: number, track: number): SearchResult {
  return {
    page_id: "p1",
    page_title: "Runbook",
    space_name: "Ops",
    headline: "a <mark>runbook</mark>",
    source: "fulltext",
    url: "/spaces/s1/pages/p1",
    ai_cost_usd: track,
    own_ai_cost_usd: own,
    total_ai_cost_usd: own + track,
  };
}

// semanticRow builds the OTHER shape merge() produces: a vector hit whose row carries no cost,
// so every cost key is ABSENT. Written as an explicit literal rather than as `row()` minus
// keys, so it cannot silently acquire a cost field if `row` grows one.
//
// ⚠ `page_title` AND `space_name` WERE FILLED HERE BEFORE THE WIRE FILLED THEM, AND THAT IS WHY
// THE BLANK ROW COULD NOT BE RED IN THIS SUITE. merge()'s semantic branch set neither, so the
// server sent `""` for both while this fixture said "Semantic"/"Ops" — and SearchModal draws a
// hit's identity as exactly those two spans, so the reader got a line with nothing written on
// it. A fixture MORE GENEROUS than the wire makes the renderer unfalsifiable. The server now
// carries both (internal/search/semantic.go: `p.title` off the already-joined pages row, `sp.name`
// off the same join the full-text half uses), measured in
// internal/search/semanticrow_realpg_test.go — so these two values are backed by the wire rather
// than by this file's optimism. `headline` stays "" because the wire still does not send one.
function semanticRow(): SearchResult {
  return {
    page_id: "p2",
    page_title: "Semantic",
    space_name: "Ops",
    headline: "",
    source: "semantic",
    similarity: 0.9,
    // ⚠ THIS WAS `/pages/p2` AND THAT WAS THE WIRE SHAPE, NOT AN ARBITRARY FIXTURE VALUE — a
    // semantic-only hit really was built with no space, and the app has no `/pages/:id` route, so
    // clicking one landed on NotFoundView. The server now carries the space on that row
    // (internal/search/semantic.go), and a fixture still holding the old spelling would be this
    // suite quietly asserting the defect is normal.
    url: "/spaces/s1/pages/p2",
  };
}

beforeEach(() => {
  rows = [];
  localStorage.clear();
});

describe("the search list reports what a document actually cost", () => {
  // D is what holds the headline to being the TOTAL rather than a copy of either addend: with
  // A/B/C alone, "show own" and "show track" are each right on two of the three.
  const cases = [
    { name: "A own-funded", own: 12.34, track: 0, total: "12.34" },
    { name: "B track-funded", own: 0, track: 1.5, total: "1.50" },
    { name: "D both halves", own: 12.34, track: 1.5, total: "13.84" },
  ];

  for (const c of cases) {
    it(`${c.name}: the row shows the TOTAL ($${c.total}), not one half`, () => {
      rows = [row(c.own, c.track)];
      renderModal();

      // The badge must EXIST. For case A it did not: the gate read the Track half, so the
      // document whose whole spend was its own AI writing showed no cost at all.
      const badge = screen.queryByTestId("search-cost");
      expect(
        badge,
        `no cost shown for a search hit that cost $${c.total} ` +
          `(own ${c.own} + track ${c.track}). The badge gate is reading one addend.`,
      ).not.toBeNull();

      expect(
        badge?.textContent,
        `the search row's cost is not the total for own ${c.own} + track ${c.track}`,
      ).toContain(`$${c.total}`);

      // The number claims to be a sum; the breakdown is what makes that claim checkable on
      // the row itself rather than asserted in a comment.
      expect(badge?.getAttribute("title")).toContain(`$${c.own.toFixed(2)}`);
      expect(badge?.getAttribute("title")).toContain(`$${c.track.toFixed(2)}`);
    });
  }

  it("C genuinely free: no cost shown — a document that cost nothing must not grow a number", () => {
    // ONE-DIRECTIONAL AND DELIBERATE. Without this, "always render the badge" passes every
    // case above, and the fix for an inverted gate would be no gate at all.
    rows = [row(0, 0)];
    renderModal();
    expect(screen.queryByTestId("search-cost")).toBeNull();
    expect(screen.queryByTestId("search-cost-unknown")).toBeNull();
  });

  it("E semantic-only: NOT REPORTED is shown as its own state, never as $0.00", () => {
    rows = [semanticRow()];
    renderModal();

    // Not a fabricated zero. This is the failure a renderer written by analogy to the page
    // screen produces: `(r.total_ai_cost_usd ?? 0).toFixed(2)` renders "$0.00" for a document
    // NOBODY LOOKED AT.
    expect(screen.queryByTestId("search-cost")).toBeNull();
    const row0 = screen.getByRole("button", { name: /Semantic/ });
    expect(
      row0.textContent,
      "a semantic-only hit rendered a currency amount; no pages row was read for it, so any " +
        "number here is a claim about a document nobody looked at",
    ).not.toMatch(/\$\d/);

    // And it is DISTINGUISHABLE from state C. This assertion is the one that makes the
    // decision above real rather than a comment: without it, "render nothing when absent"
    // passes, and that is exactly the measured status quo where an unmeasured row reads free.
    const unknown = screen.queryByTestId("search-cost-unknown");
    expect(
      unknown,
      "a semantic-only hit (cost NOT REPORTED) renders identically to a genuinely free " +
        "document (cost measured at zero). Those are different facts and the wire keeps them " +
        "apart with a nil/0 pointer distinction; the renderer must not collapse them.",
    ).not.toBeNull();
    expect(unknown?.getAttribute("title") ?? "").toMatch(/not reported/i);
  });

  it("C and E do not render the same bytes — the two states are told apart on screen", () => {
    // The pair, asserted as a pair. Each single-state test above can be satisfied by a
    // renderer that is wrong about the other one; this is the one that cannot.
    rows = [row(0, 0)];
    const free = renderModal();
    const freeText = free.getByRole("button", { name: /Runbook/ }).textContent;
    free.unmount();

    rows = [semanticRow()];
    const unknownRender = renderModal();
    const unknownText = unknownRender.getByRole("button", { name: /Semantic/ }).textContent;

    expect(freeText).not.toBeNull();
    expect(unknownText).not.toBeNull();
    // Compare only the part that is about cost — the titles differ by construction.
    expect(freeText).not.toContain("—");
    expect(unknownText).toContain("—");
  });
});
