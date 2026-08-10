package analytics_test

// RECORDING A VIEW MUST NOT RESET THE PAGE'S FRESHNESS CLOCK.
//
// GetStalePages (internal/page/store.go) selects on `updated_at < NOW() - INTERVAL '1 day' *
// stale_after_days`, so anything that touches pages.updated_at decides whether a document ever
// reaches the "needs review" list. The view bump must therefore leave that column alone — and
// this is not a hypothetical: the copy of the bump deleted in this commit
// (page.Store.RecordView) DID set it, and a page 30 days past a 7-day staleness window dropped
// off the list after a single view. Measured, both halves in one run, both view_count 0 -> 1.
//
// ⚠ THIS TEST PASSES ON THE UNMODIFIED TREE BY CONSTRUCTION — the live bump is already
// correct, so it pins a status quo and has NO red-first moment. Its entire justification is
// control S1 in scripts/w31-viewbump-owner-controls.py: `updated_at = NOW()` added to the live
// statement, which reds this and NOTHING else in the repo.
//
// ⚠ AND IT IS THE HALF THE CENSUS CANNOT SEE. TestViewCountBump_HasExactlyOneWriter counts
// writers; it cannot tell a correct statement from a divergent one. This one reads the
// behaviour and cannot see a second copy that no route reaches yet. Neither is worth much
// alone — the defect was exactly a second copy that was divergent AND unreachable. 7/7 controls
// as predicted, verdict read as the set of assertions that fired: S1 (the extra updated_at on
// the LIVE statement) reds ONLY this test; S3 (the divergent copy restored, unreachable) reds
// ONLY the census.
//
// ⚠ ONE ASSERTION HERE IS EARNED BY NOTHING AND THE HARNESS PRINTS IT AS SUCH. The "the view
// landed" precondition reds under S2 (`+ 1` -> `+ 0`) and S4 (the statement removed) — but so
// does analytics's own TestRecordView_CrossTenant_GateHoldsWithoutEnforcer_RealPG, which reads
// the counter back. So this write is NOT unheld, and that precondition is a guard against a
// vacuous run of THIS test rather than coverage of anything. My first prediction said only my
// own guards would speak; it was incomplete and is corrected in the harness rather than
// retargeted.

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func TestRecordedView_DoesNotResetTheStalenessClock_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")

	pages := page.NewStore(d.Pool)
	views := analytics.NewStore(d.Pool)

	// Two pages, identically stale: 30 days since the last edit, a 7-day window.
	mk := func(title string) string {
		id := d.Page(t, ws, alice, title)
		if _, err := d.Pool.Exec(ctx,
			`UPDATE pages SET stale_after_days = 7, updated_at = NOW() - INTERVAL '30 days'
             WHERE id = $1`, id); err != nil {
			t.Fatalf("seed stale %s: %v", title, err)
		}
		return id
	}
	// The oracle is the product's own stale list — that IS the feature under test — but the
	// preconditions below are read with SQL against the pool so a seed that never landed
	// cannot make "not on the list" read as a working reset.
	onList := func(id string) bool {
		out, err := pages.GetStalePages(ctx, ws)
		if err != nil {
			t.Fatalf("GetStalePages: %v", err)
		}
		for _, p := range out {
			if p.ID == id {
				return true
			}
		}
		return false
	}
	viewCount := func(id string) int {
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT view_count FROM pages WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("read view_count: %v", err)
		}
		return n
	}

	viewed := mk("Viewed")
	edited := mk("Edited")

	// PRECONDITION: both are on the list before anything happens. Without this, every
	// assertion below passes on a fixture that was never stale to begin with.
	if !onList(viewed) || !onList(edited) {
		t.Fatalf("fixture is wrong: seeded pages are not on the stale list (viewed=%v edited=%v)",
			onList(viewed), onList(edited))
	}

	if err := views.RecordView(ctx, analytics.PageView{
		PageID: viewed, WorkspaceID: ws, ViewerID: "mbr-reader", Duration: 60,
	}); err != nil {
		t.Fatalf("RecordView: %v", err)
	}

	// THE VIEW LANDED. A bump that silently did nothing would satisfy the staleness assertion
	// below for entirely the wrong reason.
	if got := viewCount(viewed); got != 1 {
		t.Fatalf("view_count after one view = %d, want 1 — the bump did not happen, so the "+
			"assertion below would be vacuous", got)
	}

	// THE MEASUREMENT.
	if !onList(viewed) {
		t.Fatalf("READING a page dropped it off the stale list. pages.updated_at is what " +
			"GetStalePages keys on, so a view that touches it means the documents people " +
			"actually read are the ones that never get flagged for review.")
	}

	// POSITIVE CONTROL, IN THE SAME RUN: the list DOES respond to the thing it is about. An
	// EDIT clears it. Without this, a stale list that was simply broken — never clearing for
	// anyone — would pass the assertion above.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET updated_at = NOW() WHERE id = $1`, edited); err != nil {
		t.Fatalf("seed edit: %v", err)
	}
	if onList(edited) {
		t.Fatalf("an edited page is STILL on the stale list — the instrument cannot see the " +
			"clock being reset at all, so the assertion above proves nothing")
	}
}
