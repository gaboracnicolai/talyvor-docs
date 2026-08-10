package page_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// NOTHING IN THIS REPO ASSERTED THAT A DELETED PAGE IS GONE.
//
// W3.1 finding (8). `ef27e3c` (#61) measured it rather than suspected it: with the
// `DELETE FROM pages WHERE id = $1` statement removed from Store.Delete, `go test ./...` against a
// real Postgres was GREEN across the WHOLE repo. Delete could stop deleting and nothing said so.
//
// #61 closed the half a mock can close — `newMockStore` now verifies its expectations, so the
// statement DISAPPEARING reddens TestDelete_ReparentsChildren. That is the limit of what a mock can
// reach, BY CONSTRUCTION: pgxmock never executes SQL. A statement that is called with exactly the
// arguments the mock expects and then removes NO ROW — a narrowed predicate, a filter that is
// always false, a WHERE against the wrong column — matches every expectation and passes. #53 is
// this repo's standing evidence that a SQL string can be wrong in a way only Postgres can report.
//
// So these tests read the ROW, not a return value, and they read it with SQL against the pool
// rather than through a Store getter — a getter is code that could be broken by the same edit, and
// an oracle that shares a code path with its subject is not an independent oracle.
//
// PROVENANCE OF THE HARNESS, STATED RATHER THAN INHERITED: testutil.New(t) gives an isolated,
// migrated database per test and FAILS (not skips) without DOCS_TEST_DATABASE_URL. d.Page seeds a
// space and a page with raw INSERTs — deliberately not through Store.Create, so a Create defect
// cannot make the subject of a Delete test disappear.

// countPages reads the row's existence straight from Postgres. Deliberately not GetByID: the
// question is whether the ROW is there, and a Store method could be filtered, scoped or broken by
// the very change this test exists to catch.
func countPages(t *testing.T, d *testutil.DB, id string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pages WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	return n
}

// ⚠ THE HEADLINE. After Delete, the row is not in the table.
//
// The mutation this exists for is NOT "the statement was removed" — #61's mock check already
// reddens on that. It is a Delete that RUNS, is called with the right id, reports no error, and
// removes nothing. Control P1 in scripts/w31-delete-row-controls.py is exactly that
// (`WHERE id = $1 AND FALSE`) and it leaves the whole mock suite GREEN.
func TestDelete_ActuallyRemovesTheRow_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := page.NewStore(d.Pool)

	// The premise, asserted rather than assumed: the row is there BEFORE the delete. Without this
	// the whole test passes vacuously against a seed that never landed, and "0 rows" would read as
	// a working delete. #61's own harness recorded that a zero from an instrument that read
	// nothing is not a measurement.
	if n := countPages(t, d, pageID); n != 1 {
		t.Fatalf("precondition: %d rows for the seeded page, want 1 — the subject never existed, "+
			"so any post-delete count below would be meaningless", n)
	}

	if err := s.Delete(ctx, pageID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if n := countPages(t, d, pageID); n != 0 {
		t.Fatalf("%d rows for the page AFTER a Delete that returned no error, want 0. The document "+
			"the owner believes is gone is still on disk and still reachable by any query that "+
			"does not filter on it.", n)
	}
}

// ⚠ THE SAME QUESTION ONE LAYER UP, because "the row is gone" and "the product says it is gone"
// are different claims and only the second is what a user sees. Kept separate from the headline so
// a failure names which of the two broke.
func TestDelete_TheDeletedPageIsNoLongerReadable_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := page.NewStore(d.Pool)

	if _, err := s.GetByID(ctx, pageID); err != nil {
		t.Fatalf("precondition: GetByID before the delete: %v — the subject was never readable, so "+
			"the assertion below could not fail", err)
	}
	if err := s.Delete(ctx, pageID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetByID(ctx, pageID); err == nil {
		t.Fatal("GetByID SUCCEEDED for a page that was just deleted — the delete reported success " +
			"and the document is still served")
	}
}
