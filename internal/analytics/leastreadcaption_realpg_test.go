package analytics_test

// THE EVIDENCE THAT "All pages have plenty of traffic." CAN ONLY EVER APPEAR WHEN IT IS FALSE.
//
// `Analytics.tsx` renders the least-read cohort through `PageList`, whose empty caption is the
// constant "All pages have plenty of traffic.". This file establishes, through the SHIPPED ROUTE
// on real Postgres, the backend rule that makes that string unreachable-when-true:
//
//	    least_read_pages == []   ⟺   total_views == 0
//
// Both come from ONE slice. `GetWorkspaceStats` builds `visible` from the ranked query — pages
// with at least one view in the window, minus templates, minus what the caller may not read —
// then fills `LeastReadPages` from that slice AND sums `TotalViews` over that same slice. Every
// ranked row carries `COUNT(*) >= 1`, so a non-empty `visible` forces a non-zero total, and an
// empty one forces both to zero together. The cohort is therefore empty in exactly one state:
// NOTHING THE CALLER CAN SEE HAS BEEN READ AT ALL — which is the state in which "all pages have
// plenty of traffic" is a false statement about the workspace.
//
// ⚠ WHY THIS IS A REAL-PG TEST AND NOT AN ARGUMENT IN A COMMENT. The equivalence is a claim about
// a Go slice, a SQL predicate and an authorization engine acting together, and I can read all
// three and still be wrong about their product. [HIDDEN-TRAFFIC] below is the case that would
// break it if the total were sourced anywhere else — real readership exists, the caller may not
// see any of it — and the store's own comment says the total is summed from surviving rows
// PRECISELY so an unfiltered total never sits beside a filtered list. That comment is the reason
// the equivalence survives; this file is the measurement that it does.
//
// ⚠ WHAT THIS FILE DOES NOT CLAIM: that the cohort SHOULD be empty in that state, or what the
// screen ought to say instead. It pins the backend rule only. The caption itself is corrected in
// Analytics.leastread.test.tsx, which cites this file for why the branch it deletes is not merely
// wrong-sometimes but unreachable-when-true — and this repository has repeatedly found that a
// branch kept alive for a caller that does not exist is its own defect.
//
// THE CASES:
//
//	[NO-TRAFFIC]      pages, zero views       cohort []      AND total_views 0   ← the false state
//	[SOME-TRAFFIC]    one view                cohort non-[]  AND total_views > 0 ← the true state
//	[HIDDEN-TRAFFIC]  views only on a page    cohort []      AND total_views 0   ← both fall to
//	                  the caller cannot read                                       zero TOGETHER

import (
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

func TestLeastReadCohortIsEmptyExactlyWhenThereIsNoVisibleTraffic_RealPG(t *testing.T) {
	// ── [NO-TRAFFIC] ────────────────────────────────────────────────────────────────────
	// A workspace with real documents and not one view. This is the state the caption is
	// displayed in, and the state in which it is false.
	t.Run("NO-TRAFFIC", func(t *testing.T) {
		d := testutil.New(t)
		f := newAnalyticsFixture(t, d)
		sp := seedSpaceA(t, d, f.ws, f.alice, "public", false)
		seedPageA(t, d, f.ws, sp, f.alice, "Runbook")
		seedPageA(t, d, f.ws, sp, f.alice, "Glossary")

		got := f.rollup(t, "alice@example.com", f.alice)

		if len(got.LeastReadPages) != 0 {
			t.Fatalf("[NO-TRAFFIC] least_read_pages = %d rows, want 0", len(got.LeastReadPages))
		}
		if got.TotalViews != 0 {
			t.Errorf("[NO-TRAFFIC] total_views = %d, want 0 — the cohort is empty, so the total must be too",
				got.TotalViews)
		}
		// The figure that makes the caption's falsity concrete rather than pedantic: the same
		// response says these documents have never been read.
		if got.NeverRead == 0 {
			t.Errorf("[NO-TRAFFIC] never_read_count = 0 with two unread pages seeded — the fixture is not in the state this case is about")
		}
	})

	// ── [SOME-TRAFFIC] ──────────────────────────────────────────────────────────────────
	// The positive control, and it is the one that stops the whole file being satisfied by a
	// roll-up that is empty for some unrelated reason (a broken fixture, a window that selects
	// nothing, an authorization engine that denies everything). ONE view has to flip BOTH.
	t.Run("SOME-TRAFFIC", func(t *testing.T) {
		d := testutil.New(t)
		f := newAnalyticsFixture(t, d)
		sp := seedSpaceA(t, d, f.ws, f.alice, "public", false)
		pg := seedPageA(t, d, f.ws, sp, f.alice, "Runbook")
		seedViewsA(t, d, f.ws, pg, "alice", 1)

		got := f.rollup(t, "alice@example.com", f.alice)

		if len(got.LeastReadPages) == 0 {
			t.Fatalf("[SOME-TRAFFIC] least_read_pages is empty after a view was recorded — the fixture never reached the ranking")
		}
		if got.TotalViews <= 0 {
			t.Errorf("[SOME-TRAFFIC] total_views = %d with a non-empty cohort, want > 0", got.TotalViews)
		}
	})

	// ── [HIDDEN-TRAFFIC] ────────────────────────────────────────────────────────────────
	// The case that would break the equivalence if `total_views` came from a workspace-wide
	// COUNT instead of the surviving rows: bob is a member of the workspace but has no grant on
	// alice's PRIVATE space, and every view in the workspace is on a page inside it. Readership
	// is real; none of it is bob's to see. Both figures must fall to zero TOGETHER — if the
	// total survived the filter, bob would read "0 pages listed" beside a non-zero total, which
	// is the disclosure the store's comment says it is avoiding, and the equivalence this file
	// pins would not hold.
	t.Run("HIDDEN-TRAFFIC", func(t *testing.T) {
		d := testutil.New(t)
		f := newAnalyticsFixture(t, d)
		priv := seedSpaceA(t, d, f.ws, f.alice, "private", true)
		pg := seedPageA(t, d, f.ws, priv, f.alice, "Salaries")
		seedViewsA(t, d, f.ws, pg, "alice", 4)

		// Alice can see it — this is what proves the traffic EXISTS and the fixture is real,
		// rather than the whole case passing because nothing was ever recorded.
		asAlice := f.rollup(t, "alice@example.com", f.alice)
		if len(asAlice.LeastReadPages) == 0 || asAlice.TotalViews == 0 {
			t.Fatalf("[HIDDEN-TRAFFIC] alice sees least_read=%d total_views=%d — the traffic this case hides was never recorded",
				len(asAlice.LeastReadPages), asAlice.TotalViews)
		}

		asBob := f.rollup(t, "bob@example.com", f.bob)
		if len(asBob.LeastReadPages) != 0 {
			t.Fatalf("[HIDDEN-TRAFFIC] bob's least_read_pages = %d rows, want 0 — he has no grant on that space",
				len(asBob.LeastReadPages))
		}
		if asBob.TotalViews != 0 {
			t.Errorf("[HIDDEN-TRAFFIC] bob's least_read_pages is EMPTY but total_views = %d — a filtered list beside an unfiltered total breaks the equivalence the caption's correction rests on",
				asBob.TotalViews)
		}
	})
}
