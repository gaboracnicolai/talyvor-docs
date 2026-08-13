package analytics_test

// EVERY ROW OF THE WORKSPACE ROLL-UP REPORTED `"unique_viewers":0,"avg_duration_sec":0` FOR PAGES
// THAT HAD BOTH, AND THE ROUTE ONE OVER ANSWERED THE TRUE NUMBERS FOR THE SAME PAGE IN THE SAME
// WINDOW.
//
// `GetWorkspaceStats`' ranked query selects `pv.page_id, MAX(p.title), COUNT(*)` and nothing else,
// then scans those three into a `ReadStats` — a type with SIX reportable figures. The other three
// are never touched, so every row of `most_read_pages` and `least_read_pages` carried the zero
// value of its Go field.
//
// ⚠⚠ THE THREE UNTOUCHED FIELDS DO NOT FAIL THE SAME WAY, AND THAT IS THE WHOLE ARGUMENT.
// `LastViewedAt` is a `*time.Time` with `omitempty`, so an untouched one is ABSENT from the JSON —
// it says "not reported" and says it honestly. `UniqueViewers` and `AvgDurationSec` are bare
// `int`s: an untouched one serialises as `0`, which is a MEASUREMENT. "Nobody distinct read this
// page" and "this surface did not count" are the same bytes. That is the distinction
// `search.Result`'s own comment already draws for the three cost fields in this repository
// ("`omitempty` on a float64 deletes the field when the value is 0, so 'this page cost nothing'
// and 'this surface did not report a cost' were the same bytes… nil means not reported; 0 means
// measured and zero").
//
// ⚠ THE FIX IS THE ONE `SemanticResult` TOOK, NOT POINTERS: the numbers are one aggregate away in
// a query that is ALREADY grouping exactly these rows over exactly this window. `COUNT(DISTINCT
// pv.viewer_id)`, `AVG(pv.duration_sec)` and `MAX(pv.created_at)` cost no extra scan and no extra
// round trip — the column was in the GROUP BY and missing from the SELECT list. Making the row
// TRUE is strictly better than making it silent, and it makes the two routes agree instead of
// leaving one of them fabricating.
//
// ⚠ WHAT WAS NOT ON A SCREEN, SAID SO NOBODY OVERSTATES THIS: `Analytics.tsx`'s `PageList` renders
// only `{title}` and `{total_views}`, so no false number was drawn TODAY. `api/analytics.ts`
// declares `unique_viewers` and `avg_duration_sec` as REQUIRED numbers on the same `ReadStats` the
// per-page tab renders in full, so the next consumer to read one off a roll-up row prints a
// fabricated zero — and the API is a public surface with two MCP-adjacent readers already.
//
// THE CASES:
//
//	[ROLLUP-AGREES]   the row's three figures equal the per-page route's, same page, same window
//	[DISTINCT]        two views by ONE viewer count as ONE unique viewer  ← not COUNT(*)
//	[WINDOW]          a view older than the window is in NEITHER figure   ← not all-time
//	[TOTAL-KEPT]      total_views still counts every view in the window   ← the existing figure
//	[LEAST-SAME]      the least-read cohort carries the same figures      ← one query, two ends
//	[RANK-KEPT]       the ranking is still by total views                 ← positive control

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/testutil"
)

// seedViewAged writes ONE view at a chosen age, so the window can be crossed deliberately.
func seedViewAged(t *testing.T, d *testutil.DB, wsID, pageID, viewer string, dur, daysAgo int) {
	t.Helper()
	if _, err := d.Pool.Exec(t.Context(),
		`INSERT INTO page_views (page_id, workspace_id, viewer_id, viewer_name, duration_sec, created_at)
		 VALUES ($1, $2, $3, $4, $5, NOW() - INTERVAL '1 day' * $6)`,
		pageID, wsID, viewer, viewer, dur, daysAgo,
	); err != nil {
		t.Fatalf("seed aged view: %v", err)
	}
}

func rowFor(t *testing.T, rows []analytics.ReadStats, pageID string) analytics.ReadStats {
	t.Helper()
	for _, r := range rows {
		if r.PageID == pageID {
			return r
		}
	}
	t.Fatalf("page %s absent from the cohort (%d rows)", pageID, len(rows))
	return analytics.ReadStats{}
}

// rowFor2 is rowFor's non-fatal twin. [WINDOW] asserts a page is ABSENT from a cohort, and rowFor
// calls t.Fatalf on a miss — an absence assertion cannot be written with a lookup that dies when
// the thing is correctly absent.
func rowFor2(rows []analytics.ReadStats, pageID string) *analytics.ReadStats {
	for i := range rows {
		if rows[i].PageID == pageID {
			return &rows[i]
		}
	}
	return nil
}

func TestWorkspaceRollupFiguresAreMeasured_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	sp := seedSpaceA(t, d, f.ws, f.alice, "public", false)
	busy := seedPageA(t, d, f.ws, sp, f.alice, "Runbook")
	quiet := seedPageA(t, d, f.ws, sp, f.alice, "Glossary")

	// busy: 3 DISTINCT viewers, 4 views, durations 4+10+22+10 = 46 → avg 11 (int division in SQL).
	// carol views it twice, which is what separates COUNT(DISTINCT) from COUNT(*).
	seedViewAged(t, d, f.ws, busy, "alice", 4, 0)
	seedViewAged(t, d, f.ws, busy, "bob", 10, 1)
	seedViewAged(t, d, f.ws, busy, "carol", 22, 2)
	seedViewAged(t, d, f.ws, busy, "carol", 10, 3)
	// …and ONE view far outside the 30-day window, by a FOURTH viewer with a wild duration. It
	// must not reach any figure. Without it, "computed over all time" passes every case here.
	seedViewAged(t, d, f.ws, busy, "dave", 900, 400)
	// quiet: 1 view, so the ranking has two rows and an order.
	seedViewAged(t, d, f.ws, quiet, "alice", 6, 0)
	// ancient: read heavily, but ALL of it 400 days ago. It must not rank at all — this is the
	// only case in the file that no distinctness, averaging or cross-route agreement can satisfy.
	ancient := seedPageA(t, d, f.ws, sp, f.alice, "Old Policy")
	for i := 0; i < 9; i++ {
		seedViewAged(t, d, f.ws, ancient, "erin", 30, 400)
	}

	rollup := f.rollup(t, "alice@example.com", f.alice)

	// ── [ROLLUP-AGREES] the same page, the same window, through the other route ───────
	code, body := f.get(t, "alice@example.com", f.alice, "/v1/spaces/"+sp+"/pages/"+busy+"/analytics")
	if code != http.StatusOK {
		t.Fatalf("[ROLLUP-AGREES] per-page route = %d, want 200: %s", code, body)
	}
	var perPage analytics.ReadStats
	if err := json.Unmarshal([]byte(body), &perPage); err != nil {
		t.Fatalf("[ROLLUP-AGREES] decode per-page: %v (%s)", err, body)
	}
	row := rowFor(t, rollup.MostReadPages, busy)

	if row.UniqueViewers != perPage.UniqueViewers {
		t.Errorf("[ROLLUP-AGREES] most_read_pages row reports unique_viewers=%d, the per-page route reports %d for the SAME page in the SAME window — one of these two routes is fabricating",
			row.UniqueViewers, perPage.UniqueViewers)
	}
	if row.AvgDurationSec != perPage.AvgDurationSec {
		t.Errorf("[ROLLUP-AGREES] most_read_pages row reports avg_duration_sec=%d, the per-page route reports %d for the same page",
			row.AvgDurationSec, perPage.AvgDurationSec)
	}
	switch {
	case row.LastViewedAt == nil && perPage.LastViewedAt != nil:
		t.Errorf("[ROLLUP-AGREES] most_read_pages row omits last_viewed_at; the per-page route reports %s", perPage.LastViewedAt)
	case row.LastViewedAt != nil && perPage.LastViewedAt != nil &&
		row.LastViewedAt.Sub(*perPage.LastViewedAt).Abs() > time.Second:
		t.Errorf("[ROLLUP-AGREES] most_read_pages row reports last_viewed_at=%s, the per-page route %s",
			row.LastViewedAt, perPage.LastViewedAt)
	}

	// ── [DISTINCT] carol read it twice; she is ONE unique viewer ─────────────────────
	if row.UniqueViewers != 3 {
		t.Errorf("[DISTINCT] unique_viewers=%d for a page read by alice, bob and carol (carol twice) — want 3; COUNT(*) would give 4",
			row.UniqueViewers)
	}
	// ── [WINDOW] dave's 900-second view is 400 days old and must reach nothing ───────
	//
	// ⚠ THIS CASE USED TO CARRY A SECOND ASSERTION, `unique_viewers > 3`, AND CONTROL C2 DELETED
	// IT. C2 drops the DISTINCT from the viewer count, which makes the figure 4 — so `> 3` fired
	// [WINDOW] for a reason that has nothing to do with the window, and the tag stopped naming
	// the thing it caught. It was also fully subsumed by [DISTINCT]'s `!= 3`. A tag that fires
	// for two unrelated causes is a tag that cannot tell you which one happened.
	//
	// [WINDOW] now rests on the figure only an out-of-window row can move (the average) plus the
	// dedicated page below, whose ENTIRE readership is outside the window.
	// (4+10+22+10)/4 = 11.5 → 12. ⚠ I PREDICTED 11 AND THE DATABASE SAID 12: Postgres AVG over
	// integers is numeric, and `::int` ROUNDS half away from zero rather than truncating. The
	// literal is the MEASURED value, not the arithmetic I expected; [ROLLUP-AGREES] above is what
	// makes it right either way, because it compares the two routes rather than a constant.
	// dave's 400-day-old 900s view would drag this to ~189.
	if row.AvgDurationSec != 12 {
		t.Errorf("[WINDOW]/[ROLLUP-AGREES] avg_duration_sec=%d, want 12 over the four in-window views (4,10,22,10); including the 400-day-old 900s view gives ~189",
			row.AvgDurationSec)
	}

	// ── [WINDOW] a page whose ENTIRE readership predates the window must not rank ────
	// Purely about the window predicate: no distinctness, no averaging, no agreement between
	// routes can make this pass or fail. It is the assertion C3 exists to trip.
	if stale := rowFor2(rollup.MostReadPages, ancient); stale != nil {
		t.Errorf("[WINDOW] a page whose only views are 400 days old ranks in most_read_pages with total_views=%d — the ranked query is not honouring the %d-day window",
			stale.TotalViews, 30)
	}
	if stale := rowFor2(rollup.LeastReadPages, ancient); stale != nil {
		t.Errorf("[WINDOW] the same page ranks in least_read_pages with total_views=%d", stale.TotalViews)
	}

	// ── [TOTAL-KEPT] the figure that already worked must not have moved ──────────────
	if row.TotalViews != 4 {
		t.Errorf("[TOTAL-KEPT] total_views=%d, want 4 — every in-window view, not the distinct ones", row.TotalViews)
	}
	if rollup.TotalViews != 5 {
		t.Errorf("[TOTAL-KEPT] workspace total_views=%d, want 5 (4 on Runbook + 1 on Glossary)", rollup.TotalViews)
	}

	// ── [LEAST-SAME] both cohorts come from ONE query and must carry one answer ──────
	least := rowFor(t, rollup.LeastReadPages, busy)
	if least.UniqueViewers != row.UniqueViewers || least.AvgDurationSec != row.AvgDurationSec {
		t.Errorf("[LEAST-SAME] least_read_pages reports (%d viewers, %ds avg) for the page most_read_pages reports (%d, %ds)",
			least.UniqueViewers, least.AvgDurationSec, row.UniqueViewers, row.AvgDurationSec)
	}

	// ── [RANK-KEPT] positive control: the ordering is still by total views ───────────
	if len(rollup.MostReadPages) != 2 || rollup.MostReadPages[0].PageID != busy {
		t.Errorf("[RANK-KEPT] most_read_pages is %d rows headed by %q; want 2 headed by the 4-view page (the 400-day-old page must not rank)",
			len(rollup.MostReadPages), func() string {
				if len(rollup.MostReadPages) == 0 {
					return ""
				}
				return rollup.MostReadPages[0].Title
			}())
	}
	if q := rowFor(t, rollup.MostReadPages, quiet); q.TotalViews != 1 || q.UniqueViewers != 1 {
		t.Errorf("[RANK-KEPT] the 1-view page reports total_views=%d unique_viewers=%d, want 1 and 1 — a fix that hard-codes the busy page's numbers fails here",
			q.TotalViews, q.UniqueViewers)
	}
}
