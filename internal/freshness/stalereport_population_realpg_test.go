package freshness_test

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/testutil"
	"github.com/talyvor/docs/internal/trackintegration"
)

// The workspace stale report's population is ONE SQL predicate, and both of its consumers used
// to claim a second criterion it cannot apply.
//
// ⚠⚠ THIS GUARD PASSES ON AN UNMODIFIED TREE FOR ITS BEHAVIOURAL HALF AND SAYS SO. It pins a
// boundary that holds today, so the assertions alone are NOT evidence that the boundary is real
// — scripts/w31-stalereport-population-c4a2.py is, and it is where the catchers are predicted
// before the run. The DESCRIPTION half ([DESC-*]) is genuinely red-first: it fails on the tree
// as it stood at 9e0ac54.
//
// ⚠ WHY NOTHING IN internal/freshness COULD SEE THIS. Every existing test in this package wires
// `fakePageStore`, whose GetStalePages returns a canned slice — so the author hands the engine
// whatever page they want to reason about, INCLUDING one with stale_after_days = 0 and closed
// linked issues (engine_test.go:130 does exactly that, and is correct about the per-page route).
// The predicate that defines the report's population is SQL this package never executed. The
// fake was not too weak at asserting; it replaced the very thing the claim was about.
//
// ⚠⚠ AND THE SCOPE OF THAT GAP IS NARROWER THAN IT LOOKS — MEASURED, BECAUSE THE FIRST VERSION
// OF THIS HEADER OVERSTATED IT. Widening GetStalePages to `WHERE workspace_id = $1` with every
// guard in this merge deleted is NOT invisible to the repository: the full suite goes red in
// two packages — internal/page's TestGetStalePages_FilterByTTL, which pins the predicate
// directly, plus three staleness-clock tests in page/analytics. THE SQL WAS ALWAYS GUARDED.
// What was not guarded is the pair of sentences DESCRIBING it — get_stale_pages's tool
// description and StalePages.tsx's header both promised a second criterion — and the
// cross-route disagreement below. A predicate can be pinned to the line while every consumer
// tells the user something else about it, and no assertion over the predicate can see that.
// This file is the consumer-facing half; TestGetStalePages_FilterByTTL is the predicate half.
//
// ⚠ [CONTROL] IS LOAD-BEARING, NOT DECORATION. Two of the four assertions below are ABSENCES,
// and an absence proves nothing if the fixture never had links or never reached Track. [CONTROL]
// is a page identical to the absent ones in every respect except that it has ALSO blown a TTL:
// it must come back present AND carrying linked_issues_closed = 2. Only then is
// [NO-TTL-ABSENT] the population boundary rather than measured blindness.
//
// ⚠ [CONTROL] IS A Fatalf, SO [NO-TTL-PERPAGE-FLAGS] IS NOT EVALUATED WHEN THE CONTROL DIES —
// stated rather than implied, because the two read like independent assertions and are not.
// The ordering is deliberate (with a broken control the absences below prove nothing), and it
// was the control run that showed it: C5 fired [CONTROL] ALONE where two tags were predicted.
// An assertion that only ever runs behind a passing control is not evidence on its own.
func TestStaleReport_PopulationIsTTLOnly_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO spaces (id, workspace_id, name, slug) VALUES ('sp-1','ws-1','S','s')`); err != nil {
		t.Fatalf("space: %v", err)
	}

	pages := page.NewStore(d.Pool)
	links := pagelink.NewStore(d.Pool)
	ids := map[string]string{}

	// Each page gets TWO embed-linked Track issues, both of which the Track reader below reports
	// as `done`. The only thing that differs between them is stale_after_days and age.
	mk := func(label string, staleAfter, daysAgo int) {
		created, err := pages.Create(ctx, model.Page{
			SpaceID: "sp-1", WorkspaceID: "ws-1",
			Title: "T-" + label, Content: "{}", StaleAfterDays: staleAfter,
		})
		if err != nil {
			t.Fatalf("create %s: %v", label, err)
		}
		ids[label] = created.ID
		old := time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour)
		if _, err := d.Pool.Exec(ctx,
			`UPDATE pages SET updated_at=$2, created_at=$2 WHERE id=$1`, created.ID, old); err != nil {
			t.Fatalf("age %s: %v", label, err)
		}
		for _, suffix := range []string{"-1", "-2"} {
			if _, err := d.Pool.Exec(ctx,
				`INSERT INTO page_links (page_id, workspace_id, issue_id, link_type)
                 VALUES ($1,'ws-1',$2,'embed')`, created.ID, "i-"+created.ID+suffix); err != nil {
				t.Fatalf("link %s: %v", label, err)
			}
		}
	}

	mk("control", 1, 400)  // TTL declared and blown
	mk("no-ttl", 0, 400)   // no TTL — the column DEFAULT — and 400 days untouched
	mk("warning", 100, 60) // inside a live TTL, past warningRatio of it

	tr := &populationTrack{issues: map[string]*trackintegration.IssueRef{}}
	for _, real := range ids {
		tr.issues["i-"+real+"-1"] = &trackintegration.IssueRef{Status: "done"}
		tr.issues["i-"+real+"-2"] = &trackintegration.IssueRef{Status: "done"}
	}
	eng := freshness.New(pages, links, tr).WithPageRead(populationOpenGate{})

	rep, err := eng.GetStaleReport(ctx, "ws-1")
	if err != nil {
		t.Fatalf("stale report: %v", err)
	}
	seen := map[string]freshness.FreshnessReport{}
	for _, r := range rep {
		seen[r.PageID] = r
	}

	// [CONTROL] — the liveness floor. Without this the two absences below are satisfied by a
	// fixture with no links at all, or a Track reader that was never consulted.
	ctl, ok := seen[ids["control"]]
	if !ok {
		t.Fatalf("[CONTROL] the TTL-blown page is absent from its own report — the fixture is not "+
			"wired, so every absence below proves nothing (report had %d row(s))", len(rep))
	}
	if ctl.LinkedIssuesClosed != 2 || !ctl.SuggestReview {
		t.Fatalf("[CONTROL] linked_issues_closed=%d suggest_review=%v, want 2/true — the link store "+
			"or the Track reader was not reached, so the absences below are blindness",
			ctl.LinkedIssuesClosed, ctl.SuggestReview)
	}

	// [NO-TTL-ABSENT] — completed linked issues cannot ADD a page to this report.
	if _, in := seen[ids["no-ttl"]]; in {
		t.Errorf("[NO-TTL-ABSENT] a page with stale_after_days = 0 reached the stale report. The " +
			"population widened; get_stale_pages's description and StalePages.tsx's header both " +
			"state that it cannot, and must move in the same commit.")
	}

	// [NO-TTL-PERPAGE-FLAGS] — and the same engine DOES flag that page one route over. This is
	// what makes the absence a disagreement between two routes rather than an absence of signal;
	// delete it and [NO-TTL-ABSENT] is satisfied by a page nothing considers interesting.
	st, err := eng.GetStatus(ctx, ids["no-ttl"])
	if err != nil {
		t.Fatalf("[NO-TTL-PERPAGE-FLAGS] per-page freshness: %v", err)
	}
	if !st.SuggestReview || st.LinkedIssuesClosed != 2 {
		t.Errorf("[NO-TTL-PERPAGE-FLAGS] per-page suggest_review=%v closed=%d, want true/2 — the "+
			"page the report omits must still be flagged by the by-page route",
			st.SuggestReview, st.LinkedIssuesClosed)
	}

	// [WARN-ABSENT] — the engine's own warning band is unreachable through this route, which is
	// why only one of the SPA's four TONE entries can render on that screen.
	if _, in := seen[ids["warning"]]; in {
		t.Errorf("[WARN-ABSENT] a page inside a live TTL reached the stale report")
	}
	if w, err := eng.GetStatus(ctx, ids["warning"]); err != nil {
		t.Fatalf("[WARN-ABSENT] per-page freshness: %v", err)
	} else if w.Status != freshness.FreshnessWarning {
		t.Errorf("[WARN-ABSENT] per-page status = %q, want %q — the fixture no longer sits in the "+
			"warning band, so its absence from the report is not the band being unreachable",
			w.Status, freshness.FreshnessWarning)
	}

	// [ALL-STALE] — every row this route can produce is `stale`.
	for _, r := range rep {
		if r.Status != freshness.FreshnessStale {
			t.Errorf("[ALL-STALE] page %s came back %q; staleReportAll's sort documents this as "+
				"impossible", r.PageID, r.Status)
		}
	}
}

type populationTrack struct {
	issues map[string]*trackintegration.IssueRef
}

func (p *populationTrack) IsConfigured() bool { return true }
func (p *populationTrack) GetIssue(_ context.Context, _, id string) (*trackintegration.IssueRef, error) {
	return p.issues[id], nil
}

// populationOpenGate lets every page through: this test is about the POPULATION predicate, and a
// visibility filter that dropped a fixture would be indistinguishable from the boundary it
// measures. The gate itself is covered by privatespace_realpg_test.go.
type populationOpenGate struct{}

func (populationOpenGate) AuthorizePageRead(context.Context, string) (bool, bool) { return true, true }
