package analytics_test

// EQUALLY-READ PAGES ARE THE ORDINARY CASE ON THIS DASHBOARD, AND WHICH OF THEM THE WORKSPACE
// ROLL-UP DRAWS AT ALL WAS LEFT TO THE QUERY PLAN.
//
// `GetWorkspaceStats` ranks with `ORDER BY COUNT(*) DESC, pv.page_id` and its own comment says why
// the second column is there — "makes ties deterministic rather than leaving the two ends of one
// ordering to disagree about equally-viewed pages — #88's lesson on this repo's other ranked read".
// MEASURED at `8189d7b`: deleting `, pv.page_id` leaves the WHOLE 36-package suite green. The
// lesson was learned, written down and applied to the SQL — and asserted by nothing. `ListRows`
// got `rowordertie_realpg_test.go` for exactly this class one package over; the roll-up, this
// repo's other ranked read, got the comment and no test.
//
// ⚠⚠ IT IS NOT ONLY THE ORDER OF A LIST — IT IS THE MEMBERSHIP OF ONE. The ranked window is
// fetched WHOLE and capped in Go at `rollupCap` AFTER the visibility filter, and the two cohorts
// are the head and the reversed tail of that one slice. When a tied block STRADDLES either cap,
// an undefined order decides WHICH of the equally-read pages reach the dashboard and which are
// drawn nowhere — a page appearing or vanishing with no view, no edit and no permission change
// behind it. [TIE-DECIDES-MEMBERSHIP] is that half; it is not a restatement of the order.
//
// ⚠⚠ THE ACCIDENT THAT MAKES THE OBVIOUS FIXTURE INERT — AND THE FIRST VERSION OF THIS FILE HAD IT.
// `COUNT(DISTINCT pv.viewer_id)` in this statement forces a SORTED aggregate, so the plan is always
// `GroupAggregate` fed by `Sort Key: pv.page_id`: the groups arrive in page_id order for free, and
// the only thing that can disturb them is the instability of the FINAL `Sort Key: (count(*)) DESC`.
// So with the tiebreak DELETED the answer can come back ascending ANYWAY, and the first draft of
// this test PASSED against the defect on its very first run.
//
// Shape sweep, 200 seeded trials each, against this statement at the SQL level — a MODEL of the
// read, named as one: it reproduces the query but not the Go cohort-building above it. "Inert" =
// defect present, order ascending regardless:
//
//	ONE 12-way tie, head cohort only    113/200 inert   ← the natural fixture
//	ONE 2-way tie                       103/200 inert
//	ONE 4-way tie among distinct counts   16/200 inert
//	FOUR 4-way ties, BOTH cohorts read     0/500 inert
//	SIX  4-way ties, BOTH cohorts read     0/500 inert   ← this file
//
// ⚠⚠ AND HERE IS WHAT THE CONTROLS COULD NOT ESTABLISH, RECORDED BECAUSE IT WOULD BE EASY TO LEAVE
// THE TABLE ABOVE LOOKING LIKE PROOF. C5 and C8–C14 rebuilt FOUR variants of this fixture against
// the same defect — one wide tie instead of six narrow ones; the head cohort read alone; both; a
// three-group shape — expecting at least one to go inert and reproduce the model's numbers through
// the PRODUCT path. Every one of them CAUGHT it, 12/12. So:
//
//   - the accident is REAL and was observed here, not inferred: the first draft of this file (one
//     12-way tie flanked by a 5-view and a 1-view page, head cohort only) PASSED against the defect
//     on its first run, and 1/10 over ten runs;
//   - the fixture below is measured to catch it 37/37 through the product path;
//   - but WHICH of its choices denies the accident is NOT established. The narrow difference
//     between the inert first draft and the catching variants is a 12-wide tie FLANKED by distinct
//     counts in a 14-row sort, and no control isolated that.
//
// Treat the shape as measured-adequate, not as understood. If a future change makes this file
// quiet, do not assume the fixture still bites — re-run the defect against it.
//
// ⚠ A CLAIM THIS HEADER MADE AND CONTROL C4 REFUTED, LEFT IN RATHER THAN QUIETLY DROPPED: the
// fixture used to run `ANALYZE` on both tables, justified — by analogy with
// `rowordertie_realpg_test.go`'s `VACUUM` — as what denies the accident by moving the planner off
// its no-statistics estimates. C4 puts the `ANALYZE` BACK with the defect present: CAUGHT, 5/5,
// indistinguishable from C1 without it.
// The plan here is `GroupAggregate` with or without statistics, because the DISTINCT aggregate
// decides it, so the `ANALYZE` moved nothing and has been DELETED. An analogy to a neighbouring
// test is not evidence — which is the same lesson the paragraph above had to learn twice.
//
// ⚠ WHAT THE TIEBREAK BUYS IS A *DEFINED* ORDER, NOT A MEANINGFUL ONE. `pages.id` is
// `gen_random_uuid()::text`, so ascending page_id is arbitrary with respect to anything a reader
// could name — it is not creation order and does not claim to be. The whole and honest claim is
// that the same rows produce the same dashboard twice. The un-tiebroken read is not even noisy —
// MEASURED, two executions in one session agreed with each other and disagreed with page_id
// order — which is exactly why nothing catches this by flapping.
//
// THE CASES:
//
//	[TIE-IS-REAL]             the tied pages really do share a view count, read off the wire
//	[TIE-ORDER-IS-DEFINED]    equally-read pages ascend by page_id in the head cohort
//	[BOTH-ENDS-AGREE]         …and descend in the tail cohort — one ordering, read from both ends
//	[TIE-DECIDES-MEMBERSHIP]  which tied pages survive a cap is the tiebreak's answer, not the plan's
//	[RANK-STILL-PRIMARY]      COUNT(*) DESC is still the sort key; page_id only breaks its ties

import (
	"fmt"
	"sort"
	"testing"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/testutil"
)

// The fixture: SIX groups of FOUR equally-read pages, each group on its own view count. Twenty-four
// visible pages against a `rollupCap` of 10 means the head cohort ends inside group 3 and the tail
// cohort ends inside group 4 — the two straddles [TIE-DECIDES-MEMBERSHIP] reads — and four pages
// (the rest of groups 3 and 4) are drawn in NEITHER cohort, which is the product fact that makes
// membership a real question rather than a restatement of order.
const (
	tieGroups    = 6
	tieGroupSize = 4
)

func TestWorkspaceRollupRankedTieHasADefinedOrder_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	sp := seedSpaceA(t, d, f.ws, f.alice, "public", false)

	// Group g gets `tieGroups-g+1` views on every one of its pages: distinct counts BETWEEN groups
	// so `COUNT(*) DESC` fully orders the groups, identical counts WITHIN a group so only the
	// tiebreak can order their members.
	byCount := map[int][]string{}
	for g := 0; g < tieGroups; g++ {
		views := tieGroups - g
		for k := 0; k < tieGroupSize; k++ {
			id := seedPageA(t, d, f.ws, sp, f.alice, fmt.Sprintf("Tied g%d n%d", g, k))
			seedViewsA(t, d, f.ws, id, "alice", views)
			byCount[views] = append(byCount[views], id)
		}
		sort.Strings(byCount[views]) // what `ORDER BY … , pv.page_id` must produce
	}

	// ⚠ NO `ANALYZE` HERE ON PURPOSE — it was in the first draft, justified by analogy, and C4
	// measured it inert (CAUGHT 5/5 with and without). See the header.

	rollup := f.rollup(t, "alice@example.com", f.alice)

	// ── [TIE-IS-REAL] the vacuity floor, asserted BEFORE any order is ────────────────
	// Everything below is a statement about pages that SHARE a view count. If a future change gave
	// them distinct counts, `COUNT(*) DESC` alone would fully order every block and the assertions
	// would pass without the tiebreak ever being consulted. Counted off the WIRE, not off the
	// number of views seeded.
	tiedInCohort := func(rows []analytics.ReadStats) int {
		seen := map[int]int{}
		for _, r := range rows {
			seen[r.TotalViews]++
		}
		n := 0
		for _, c := range seen {
			if c > 1 {
				n++
			}
		}
		return n
	}
	if got := tiedInCohort(rollup.MostReadPages); got < 2 {
		t.Fatalf("[TIE-IS-REAL] most_read_pages holds %d groups of equally-read pages, want at least 2 "+
			"— there is no tie left for the assertions below to have an order over (cohort of %d)",
			got, len(rollup.MostReadPages))
	}
	if got := tiedInCohort(rollup.LeastReadPages); got < 2 {
		t.Fatalf("[TIE-IS-REAL] least_read_pages holds %d groups of equally-read pages, want at least 2 (cohort of %d)",
			got, len(rollup.LeastReadPages))
	}

	// ── [TIE-ORDER-IS-DEFINED] the finding, head cohort ─────────────────────────────
	// Equal `total_views` ⇒ ascending page_id. With the tiebreak deleted this is whatever the final
	// sort happened to leave behind.
	for i := 1; i < len(rollup.MostReadPages); i++ {
		for j := 0; j < i; j++ {
			a, b := rollup.MostReadPages[j], rollup.MostReadPages[i]
			if a.TotalViews == b.TotalViews && a.PageID > b.PageID {
				t.Errorf("[TIE-ORDER-IS-DEFINED] most_read_pages returns %s before %s and both have "+
					"total_views=%d — a ranked read whose ORDER BY names no unique column has NO "+
					"defined relative order, and this one is capped, so the plan is choosing the dashboard",
					a.PageID, b.PageID, a.TotalViews)
				i = len(rollup.MostReadPages) // one report is enough
				break
			}
		}
	}

	// ── [BOTH-ENDS-AGREE] the same ordering, read from the other end ────────────────
	// least_read_pages is the tail of the SAME slice, reversed, so equal-count rows must come back
	// DESCENDING here. This is the store comment's own stated reason for the tiebreak — that the
	// two ends of one ordering must not disagree about equally-viewed pages — and it is also what
	// makes the tail cohort's straddling group observable at all.
	for i := 1; i < len(rollup.LeastReadPages); i++ {
		for j := 0; j < i; j++ {
			a, b := rollup.LeastReadPages[j], rollup.LeastReadPages[i]
			if a.TotalViews == b.TotalViews && a.PageID < b.PageID {
				t.Errorf("[BOTH-ENDS-AGREE] least_read_pages returns %s before %s and both have "+
					"total_views=%d — the tail cohort is the head cohort's ordering reversed; "+
					"disagreeing about equally-viewed pages is what the tiebreak exists to stop",
					a.PageID, b.PageID, a.TotalViews)
				i = len(rollup.LeastReadPages)
				break
			}
		}
	}

	// ── [TIE-DECIDES-MEMBERSHIP] the half that is not about order ───────────────────
	// A tied group that STRADDLES a cap has only some of its pages drawn. Which ones is the
	// tiebreak's answer: the lowest page_ids at the head, the highest at the tail. The straddle is
	// LOCATED from the response rather than computed from `rollupCap`, which is unexported and not
	// this test's to know.
	straddles := 0
	assertStraddle := func(tag string, rows []analytics.ReadStats, lowest bool) {
		t.Helper()
		present := map[int][]string{}
		for _, r := range rows {
			present[r.TotalViews] = append(present[r.TotalViews], r.PageID)
		}
		for views, got := range present {
			all := byCount[views]
			if len(got) == 0 || len(got) == len(all) {
				continue // fully drawn or absent: no straddle, nothing to decide
			}
			straddles++
			want := append([]string(nil), all...)
			if lowest {
				want = want[:len(got)]
			} else {
				want = want[len(all)-len(got):]
			}
			sort.Strings(got)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				end := "highest"
				if lowest {
					end = "lowest"
				}
				t.Errorf("%s %d of the %d pages with total_views=%d are drawn, and they are %v; the "+
					"cap must fall on the %s page_ids, %v — otherwise a page is on or off the "+
					"dashboard because of the query plan, with no view, no edit and no permission "+
					"change behind it", tag, len(got), len(all), views, got, end, want)
			}
		}
	}
	assertStraddle("[TIE-DECIDES-MEMBERSHIP] most_read_pages:", rollup.MostReadPages, true)
	assertStraddle("[TIE-DECIDES-MEMBERSHIP] least_read_pages:", rollup.LeastReadPages, false)
	// The vacuity floor UNDER the membership half, and it is a real risk rather than a ritual: the
	// straddle exists only because 24 visible pages in groups of 4 happen to fall either side of
	// `rollupCap`. Raise the cap past 24 — a one-constant change in a file this test cannot see —
	// and every loop above skips, the tag goes silent, and nothing says membership stopped being
	// asserted. This is what says it.
	if straddles < 2 {
		t.Errorf("[TIE-DECIDES-MEMBERSHIP] %d tied groups straddle a cohort boundary, want 2 (one per "+
			"cohort) — with no straddle the membership assertions above skip every group and assert "+
			"NOTHING; the fixture must be re-sized to the cap it is measuring", straddles)
	}

	// ── [RANK-STILL-PRIMARY] the floor under the fix ────────────────────────────────
	// A tiebreak that became the sort key (`ORDER BY pv.page_id`) makes a defined order out of an
	// undefined one and is caught here rather than passing as "deterministic". page_id is a random
	// uuid, so it interleaves the six view-counts rather than preserving them.
	//
	// ⚠ NON-ISOLATING, MEASURED AND LABELLED RATHER THAN LEFT LOOKING LOAD-BEARING: control C3
	// reddens this tag AND the two ordering tags, because interleaving the counts also breaks the
	// within-group order. It is kept because it NAMES the reason a reader of a red run needs, not
	// because it is the assertion bearing that weight.
	for i := 1; i < len(rollup.MostReadPages); i++ {
		if rollup.MostReadPages[i-1].TotalViews < rollup.MostReadPages[i].TotalViews {
			t.Errorf("[RANK-STILL-PRIMARY] most_read_pages goes %d views then %d — total views must "+
				"still ORDER the list; page_id only breaks its ties",
				rollup.MostReadPages[i-1].TotalViews, rollup.MostReadPages[i].TotalViews)
			break
		}
	}
}
