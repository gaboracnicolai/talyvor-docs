package pagelink

// THE RECONCILER READ THE SET IT DIFFS AGAINST AND NEVER ASKED WHETHER IT GOT ALL OF IT.
//
// `SyncLinks` reads the page's CURRENT embed links into `existing`, then deletes every entry of
// `existing` the new content no longer embeds. The read loop ran to `rows.Next() == false` and
// went straight on to the diff — and `rows.Next()` returns false both when the rows ran out and
// when the stream broke. A truncated read therefore produces a SHORT `existing`, which is a
// silent UNDER-DELETION: an issue the author just removed from the document keeps its row in
// `page_links`, and `SyncLinks` returns nil.
//
// ⚠ WHY THAT ROW IS NOT INERT — IT IS AN INPUT TO A MONEY COLUMN. `IssueIDsForPage`, twenty
// lines up in this same file, is the cost syncer's read of the SAME table with the SAME
// `link_type = 'embed'` predicate; `trackintegration.Syncer.SyncPageCosts` sums those issues'
// AI cost and writes the total to `pages.ai_cost_usd`, which the page API serves as
// `total_ai_cost_usd`. A leftover embed link is therefore a Track issue the document keeps being
// billed for on screen, and it stays that way until the next content save reconciles — nobody
// gets told, because the sweep recomputes the same wrong sum every tick and calls it complete.
//
// ⚠ AND `IssueIDsForPage` IS THE CONTROL FOR "THIS IS AN OVERSIGHT, NOT A CHOICE": it ends
// `return out, rows.Err()`. So does the rest of the repository — a census of every
// `for rows.Next()` in non-test `internal/` at 3bdb186 is 36 loops, 33 of which consult
// `rows.Err()`. This was one of the three that did not.
//
// ⚠ WHAT THIS FIX DOES NOT DO, MEASURED AND STATED. `page.Store.Update` calls this as
// `_ = s.linker.SyncLinks(...)` — the error is DISCARDED at the only production call site, which
// the surrounding comment justifies as best-effort ("the next content edit will reconcile
// again"). So the fix converts a silent WRONG reconcile into a silent SKIPPED one: no partial
// diff is computed from a partial set. It does not make the failure visible to an operator, and
// nothing here should be read as claiming it does. What it does is stop the function returning
// `nil` — "reconciled" — for a reconcile that did not happen.
//
// ⚠ THE MECHANISM IS MEASURED, NOT ASSUMED — see internal/analytics/rowstream_test.go for the
// real-Postgres numbers (2 of 5 rows delivered, Err() = SQLSTATE 22012; 2 of 20 delivered under
// a statement timeout, Err() = 57014). This file uses pgxmock's CloseError, whose semantics
// match: rows delivered, then Next() false, then Err() non-nil.

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// errStreamBroke stands for the 57014 measured on the real driver.
var errStreamBroke = errors.New("canceling statement due to statement timeout (SQLSTATE 57014)")

// TestSyncLinks_ATruncatedExistingReadIsNotADiff.
//
// The fixture is the ordinary edit that exposes it: the page currently embeds i-1 and i-2, the
// author deletes BOTH from the content, and the read of `existing` breaks after i-1. The diff
// computed from that prefix deletes i-1 and leaves i-2 linked — and `page_links` is where the
// cost roll-up gets its issue list.
func TestSyncLinks_ATruncatedExistingReadIsNotADiff(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectQuery(`SELECT issue_id FROM page_links WHERE page_id`).
		WithArgs("p-1", "embed").
		WillReturnRows(pgxmock.NewRows([]string{"issue_id"}).
			AddRow("i-1").
			CloseError(errStreamBroke))

	// ⚠ NO EXEC EXPECTATIONS, AND THAT IS THE SECOND HALF OF THE ASSERTION. A reconcile that
	// knows its input was truncated must write NOTHING — not the deletion it can still justify,
	// not the insertion it can. newMockStore's ExpectationsWereMet cleanup turns any write here
	// into a loud mismatch instead of an unnoticed one.
	doc := `{"type":"doc","content":[{"type":"paragraph","content":[]}]}`
	err := store.SyncLinks(context.Background(), "p-1", "ws-1", doc, "u")

	// ── [EXISTING-READ-TRUNCATION-IS-AN-ERROR] the finding. ───────────────────────────
	if err == nil {
		t.Fatalf("[EXISTING-READ-TRUNCATION-IS-AN-ERROR] SyncLinks returned nil — 'reconciled' — " +
			"after reading a PREFIX of the page's existing embed links; every link the stream " +
			"did not deliver survives the edit, and pages.ai_cost_usd keeps summing it")
	}
	if !errors.Is(err, errStreamBroke) {
		t.Errorf("[EXISTING-READ-TRUNCATION-IS-AN-ERROR] err = %v, want it to wrap the stream "+
			"failure", err)
	}
}

// TestSyncLinks_AWholeExistingReadStillReconciles is the vacuity floor.
//
// ⚠ WITHOUT IT THE CHEAPEST WAY TO GO GREEN IS TO MAKE SyncLinks ALWAYS FAIL, which would take
// the reconciler out of service entirely and leave every page's links frozen — a strictly worse
// version of the defect. This asserts the WRITES, not merely a nil error: with i-1 and i-2 on
// disk and content that embeds i-1 and i-3, the healthy path must still insert i-3 and delete
// i-2.
func TestSyncLinks_AWholeExistingReadStillReconciles(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectQuery(`SELECT issue_id FROM page_links WHERE page_id`).
		WithArgs("p-1", "embed").
		WillReturnRows(pgxmock.NewRows([]string{"issue_id"}).
			AddRow("i-1").AddRow("i-2"))
	pool.ExpectExec(`INSERT INTO page_links`).
		WithArgs("p-1", "ws-1", "i-3", "embed", "u").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`DELETE FROM page_links`).
		WithArgs("p-1", "i-2", "embed").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	doc := `{"type":"doc","content":[{"type":"paragraph","content":[
        {"type":"issue_embed","attrs":{"issue_id":"i-1"}},
        {"type":"issue_embed","attrs":{"issue_id":"i-3"}}
    ]}]}`
	if err := store.SyncLinks(context.Background(), "p-1", "ws-1", doc, "u"); err != nil {
		t.Fatalf("[UNBROKEN-READ-STILL-RECONCILES] SyncLinks: %v — an unbroken read must still "+
			"add and remove", err)
	}
}
