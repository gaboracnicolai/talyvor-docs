package analytics_test

// THE PER-PAGE ANALYTICS ROUTE REPORTED `"title":""` FOR EVERY PAGE, AND THE ROLL-UP ROUTE
// REPORTED THE REAL TITLE FOR THE SAME PAGE IN THE SAME RESPONSE SHAPE.
//
// `analytics.ReadStats` has a `Title string \`json:"title"\`` field and TWO producers.
// `GetWorkspaceStats`' ranked query selects `MAX(p.title)` into it. `GetReadStats` — the whole
// body of GET /v1/spaces/{spaceID}/pages/{pageID}/analytics, and the source the MCP
// `get_page_analytics` tool reads — never assigns it at all, so every response down that route
// carried the Go zero value of a bare string.
//
// ⚠ THIS IS THE RESIDUE OF THE FIX rollupfigures_realpg_test.go RECORDS, ONE FIELD OVER, AND IT
// FAILS THE SAME WAY. That merge found three of `ReadStats`' six reportable figures untouched on
// the ROLL-UP side and filled them from the aggregate that was already grouping the rows, on the
// stated ground that "an untouched int serialises as 0, which is a MEASUREMENT" and that making
// the row TRUE is what makes the two routes agree. `Title` is a bare `string` on the OTHER side of
// exactly that comparison: an untouched one serialises as `""`, which is not an omission — it is
// the claim "this page is called nothing". [ROLLUP-AGREES] could not see it, because it compares
// the two routes only on the three fields that merge repaired.
//
// ⚠ WHAT IS NOT ON A SCREEN TODAY, SAID SO NOBODY OVERSTATES THIS. `Analytics.tsx`'s
// `PageAnalytics` draws its heading from a `title` PROP threaded down from the page it is already
// showing, never from `data.title`, so no user currently reads the empty string. What reads it is
// the type: `frontend/src/api/analytics.ts` declares `title: string` — REQUIRED, non-nullable — on
// the one `ReadStats` both routes return, and `PageList` already renders `{p.title ||
// p.page_id.slice(0, 8)}` off roll-up rows. The next consumer to render a per-page row prints an
// id where a name belongs, and the API is a public surface.
//
// ⚠ THE FIX IS THE ONE THE ROLL-UP TOOK — FILL IT, DO NOT HIDE IT — BUT NOT BY THE SAME MEANS,
// AND THE DIFFERENCE IS LOAD-BEARING. The roll-up's three figures were aggregates of rows the
// query already had. A title is NOT a property of page_views: the totals statement reads
// `FROM page_views WHERE page_id = $1 AND created_at > …`, and adding `JOIN pages` to it would
// turn the never-viewed case (an aggregate over zero rows, which still returns one row) into a
// NULL title, breaking the one caller shape that matters most on a readership screen. It is a
// scalar subquery on `pages` instead: no extra round trip, and a page that does not exist yields
// SQL NULL rather than a fabricated name — read through sql.NullString, so "absent" stays
// distinguishable from "measured and empty" at the scan boundary.
//
// THE CASES:
//
//	[TITLE-REPORTED]  the per-page route names the page it was asked about
//	[ROUTES-AGREE]    that name is byte-identical to the roll-up row's, same page  ← the residue
//	[NOT-A-CONSTANT]  a SECOND page reports ITS OWN title                          ← positive control
//	[UNVIEWED]        a page with no views at all still reports its title          ← the JOIN trap
//	[WIRE]            the raw bytes carry it, not just the decoded struct
//
// 5/5 controls in scripts/w31-perpagetitle-controls-a41f.py, each over the FULL package on real
// Postgres, restored in a `finally` and sha256-verified: C1 the defect → all five; C2 a constant
// title → NOT-A-CONSTANT + UNVIEWED alone; C3 the JOIN form → UNVIEWED alone; C4 the ROLL-UP side
// blinded → ROUTES-AGREE alone (so that case is a two-sided comparison, not a restatement of
// TITLE-REPORTED); C5 a real title off a fixed OTHER row → four red with NOT-A-CONSTANT GREEN,
// which is what says these two cases catch different wrongnesses and neither subsumes the other.
//
// ⚠ C3 WAS WRONG BEFORE THIS GUARD WAS, AND THE RUN SAID SO. Its first version grafted the JOIN on
// without qualifying the columns; `created_at` is on both tables, Postgres refused the statement,
// and the suite failed with NOT ONE named case red — a control reddening because the query is
// broken says nothing about whether [UNVIEWED] can fail. Corrected to the alias-qualified form a
// reviewer would actually write, it reds [UNVIEWED] alone, which is the measurement.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/testutil"
)

// perPage drives the shipped per-page route and returns both the decoded stats and the raw bytes.
// The raw half is not decoration: a wire assertion decoded through a struct cannot tell a field
// that was sent empty from one the server omitted, which is the distinction this file is about.
func perPage(t *testing.T, f *analyticsFixture, spaceID, pageID string) (analytics.ReadStats, string) {
	t.Helper()
	code, body := f.get(t, "alice@example.com", f.alice, "/v1/spaces/"+spaceID+"/pages/"+pageID+"/analytics")
	if code != http.StatusOK {
		t.Fatalf("GET per-page analytics = %d, want 200: %s", code, body)
	}
	var out analytics.ReadStats
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode per-page analytics: %v (%s)", err, body)
	}
	return out, body
}

func TestPerPageAnalyticsReportsTheTitle_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	sp := seedSpaceA(t, d, f.ws, f.alice, "public", false)

	// Two DIFFERENT titles, so nothing that returns one constant — or the wrong row — can pass.
	runbook := seedPageA(t, d, f.ws, sp, f.alice, "Runbook")
	glossary := seedPageA(t, d, f.ws, sp, f.alice, "Glossary")
	// A third that is never viewed. The roll-up's ranked query cannot produce a row for it at all,
	// so it is the case a `JOIN page_views` style fix would answer with a NULL or an empty string.
	orphan := seedPageA(t, d, f.ws, sp, f.alice, "Never Opened")

	seedViewsA(t, d, f.ws, runbook, "alice", 3)
	seedViewsA(t, d, f.ws, glossary, "bob", 1)

	// ── [TITLE-REPORTED] ─────────────────────────────────────────────────────────────
	got, raw := perPage(t, f, sp, runbook)
	if got.Title != "Runbook" {
		t.Errorf("[TITLE-REPORTED] GET /spaces/{}/pages/{}/analytics reports title=%q for a page called %q — a bare string field that no producer assigns serialises as \"\", which is the positive claim that this document has no name",
			got.Title, "Runbook")
	}
	// ── [WIRE] the bytes, not the decoded struct ─────────────────────────────────────
	if !strings.Contains(raw, `"title":"Runbook"`) {
		t.Errorf("[WIRE] the response body does not carry `\"title\":\"Runbook\"`; api/analytics.ts declares `title: string` as REQUIRED on this exact type.\nbody: %s", raw)
	}

	// ── [ROUTES-AGREE] the residue of [ROLLUP-AGREES] ────────────────────────────────
	rollup := f.rollup(t, "alice@example.com", f.alice)
	row := rowFor(t, rollup.MostReadPages, runbook)
	if row.Title != got.Title {
		t.Errorf("[ROUTES-AGREE] the roll-up row calls page %s %q and the per-page route calls it %q — one ReadStats, two routes, two answers about the same document",
			runbook, row.Title, got.Title)
	}

	// ── [NOT-A-CONSTANT] a second page must report ITS OWN title ─────────────────────
	// Without this, filling the field with any fixed string — or with a title read off the wrong
	// row — satisfies every assertion above.
	other, _ := perPage(t, f, sp, glossary)
	if other.Title != "Glossary" {
		t.Errorf("[NOT-A-CONSTANT] the route reports title=%q for the page called %q — the field is being filled from something other than the page that was asked about",
			other.Title, "Glossary")
	}

	// ── [UNVIEWED] the never-read page still knows its name ──────────────────────────
	// Its readership figures are legitimately zero; its title is not. This is the case a fix
	// grafted onto the page_views aggregate cannot answer.
	unread, _ := perPage(t, f, sp, orphan)
	if unread.Title != "Never Opened" {
		t.Errorf("[UNVIEWED] a page with no views at all reports title=%q, want %q — the title is a property of the page, not of its traffic",
			unread.Title, "Never Opened")
	}
	if unread.TotalViews != 0 {
		t.Errorf("[UNVIEWED] the never-viewed page reports total_views=%d, want 0 — the fixture is not what this case claims it is",
			unread.TotalViews)
	}
}
