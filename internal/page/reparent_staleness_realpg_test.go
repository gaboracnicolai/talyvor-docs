package page_test

// DELETING A PAGE MUST NOT RESET ITS CHILDREN'S FRESHNESS CLOCK.
//
// Store.Delete re-parents the deleted page's children onto its own parent before removing the
// row. That statement also wrote `updated_at = NOW()`, and GetStalePages selects on
//
//	updated_at < NOW() - INTERVAL '1 day' * stale_after_days
//
// so the re-parent silently cleared the staleness clock of EVERY child, for a full
// stale_after_days window, without anybody editing or verifying a single one of them.
//
// ⚠ IT IS THE FOURTH COPY OF A SEAM THIS REPO HAD ALREADY FOUND THREE TIMES AND WRITTEN DOWN.
// page.Store.RecordView set updated_at and was deleted for it (the block above GetStalePages);
// page.Store.PriceAISpend set it and had it removed (ai_spend.go, pinned by
// aispend_staleness_test.go); analytics.Store.RecordView is pinned NOT to set it
// (analytics.TestRecordedView_DoesNotResetTheStalenessClock_RealPG). Delete's re-parent was the
// writer none of those three guards can see, because each pins one statement rather than the
// column.
//
// ⚠ AND IT IS THE ONLY ONE OF THE FOUR THAT IS A BULK WRITE BY A THIRD PARTY. The other three
// re-dated ONE page as a side effect of an operation ON that page. This re-dates every child of
// the deleted page at once, and the actor is somebody deleting a DIFFERENT document — the
// children's own authors did nothing, and nothing on any surface records that their review
// clocks were reset. GetStalePages feeds the SPA's stale screen and sidebar count, the 09:00 UTC
// freshness digest (internal/freshness), GET /workspaces/{wsID}/stale, and the MCP
// get_stale_pages tool; all four went quiet together.
//
// ⚠ WHAT IS DELIBERATELY NOT CHANGED, so this is not read as an answer to the wider question.
// A user-initiated MOVE still bumps updated_at: parent_id is in Update's field list and Update
// sets the column itself. The question "does re-parenting count as updating a document" is
// therefore untouched — what is fixed is that the CASCADE from deleting a different page must
// not answer it on the child's behalf.
//
// ⚠ RED-FIRST, MEASURED: on the unmodified tree at df4ff24 this test fails at the [CLOCK]
// assertion — child updated_at 2026-07-13 -> 2026-08-12, on the stale list -> off it. The
// positive controls are scripts/w31-reparent-controls-9c4e.py: 8/8 as predicted, and [CLOCK] is
// the SOLE SPEAKER for R1 (the defect restored), so nothing else in this repository sees it.
//
// ⚠ WHAT THIS TEST DOES **NOT** EARN, printed by the harness rather than left to be assumed:
// [CONTROL] is never the only assertion that fires. Both mutations that neuter the stale list's
// response to an edit (R5, R8) also red TestPricedAISpend_DoesNotResetTheStalenessClock_RealPG,
// which carries the same in-run control. [CONTROL] is a guard against a vacuous run of THIS
// test, not coverage this file adds.

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func TestDeletingAParent_DoesNotResetItsChildrensStalenessClock_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pages := page.NewStore(d.Pool)

	// grandparent -> parent -> child. The grandparent exists so the re-parent has a non-NULL
	// destination to be checked against: a "fix" that simply deleted the UPDATE would satisfy
	// the clock assertion below and orphan every child, and [REPARENT] is what separates them.
	grandparent := d.Page(t, ws, alice, "Grandparent")
	parent := d.Page(t, ws, alice, "Parent")
	child := d.Page(t, ws, alice, "Child")
	// `unrelated` is the in-run positive control: identically stale, NOT under the deleted page.
	unrelated := d.Page(t, ws, alice, "Unrelated")

	link := func(kid, dad string) {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, `UPDATE pages SET parent_id = $1 WHERE id = $2`, dad, kid); err != nil {
			t.Fatalf("seed parent link: %v", err)
		}
	}
	link(parent, grandparent)
	link(child, parent)

	// Both leaf pages identically stale: 30 days since the last edit, a 7-day window. Seeded
	// with SQL because the point is a clock that predates the operation under test.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET stale_after_days = 7, updated_at = NOW() - INTERVAL '30 days'
		 WHERE id = ANY($1)`, []string{child, unrelated}); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	// The oracle is the product's own stale list — that IS the feature under test.
	onList := func(id string) bool {
		t.Helper()
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
	parentOf := func(id string) string {
		t.Helper()
		var p *string
		if err := d.Pool.QueryRow(ctx, `SELECT parent_id FROM pages WHERE id = $1`, id).Scan(&p); err != nil {
			t.Fatalf("read parent_id: %v", err)
		}
		if p == nil {
			return ""
		}
		return *p
	}
	updatedAt := func(id string) string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx, `SELECT updated_at::text FROM pages WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return s
	}

	// PRECONDITION. Without it every assertion below passes on a fixture that was never stale,
	// and on a tree where the parent link never landed.
	if !onList(child) || !onList(unrelated) {
		t.Fatalf("fixture is wrong: seeded pages are not on the stale list (child=%v unrelated=%v)",
			onList(child), onList(unrelated))
	}
	if got := parentOf(child); got != parent {
		t.Fatalf("fixture is wrong: child's parent_id = %q, want %q", got, parent)
	}
	before := updatedAt(child)

	if err := pages.DeleteInWorkspaces(ctx, parent, []string{ws}); err != nil {
		t.Fatalf("DeleteInWorkspaces(parent): %v", err)
	}

	// [REPARENT] The cascade still does its job. This is the assertion that makes deleting the
	// UPDATE statement a failing fix rather than a passing one.
	if got := parentOf(child); got != grandparent {
		t.Fatalf("[REPARENT] child's parent_id = %q after deleting its parent, want the "+
			"grandparent %q — the re-parent no longer runs", got, grandparent)
	}

	// [CLOCK] THE MEASUREMENT.
	if !onList(child) {
		t.Fatalf("[CLOCK] deleting a page dropped its CHILD off the stale list. "+
			"pages.updated_at is what GetStalePages keys on, so the re-parent cascade reset a "+
			"document's review clock that nobody edited and nobody verified: %s -> %s",
			before, updatedAt(child))
	}

	// [CONTROL] The instrument can still see a clock being reset — through the product's own
	// write path, not raw SQL. Without this, a stale list that was simply broken (never
	// clearing for anyone) would satisfy [CLOCK] for entirely the wrong reason.
	if _, err := pages.Update(ctx, unrelated, map[string]any{"title": "Edited by hand"}); err != nil {
		t.Fatalf("Update(unrelated): %v", err)
	}
	if onList(unrelated) {
		t.Fatalf("[CONTROL] an EDITED page is still on the stale list — the instrument cannot " +
			"see the clock being reset at all, so [CLOCK] proves nothing")
	}
}
