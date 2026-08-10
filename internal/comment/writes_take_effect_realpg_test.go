package comment_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/testutil"
)

// THREE WRITES IN THIS PACKAGE COULD RUN AND CHANGE NOTHING, AND NOTHING IN THE REPO WOULD SAY SO.
//
// W3.1 finding (9), measured rather than suspected (scripts/w31-cross-package-write-controls.py).
// `3c8fbb2` closed the half a mock can close: `newMockStore` now verifies its expectations, so a
// write STATEMENT DISAPPEARING out of store.go reddens the test that names it.
//
// That is the limit of a mock BY CONSTRUCTION — pgxmock never executes SQL and hands back whatever
// RowsAffected the fixture wrote. A statement that is CALLED with exactly the expected arguments
// and then matches NO ROW satisfies every expectation. Family P of that harness is that mutation
// (` AND FALSE` appended to the WHERE) and it left the whole repo GREEN for Resolve, Unresolve and
// Delete. #53 is this repo's standing evidence that a SQL string can be wrong in a way only
// Postgres reports.
//
// So these tests read the ROW with SQL against the pool, never through a Store getter: a getter is
// code the same edit could break, and an oracle sharing a code path with its subject is not an
// independent oracle. Each asserts its PRECONDITION first — a seed that never landed would
// otherwise make "not resolved" or "0 rows" read as a working write.

// readComment reads the three columns these writes touch, straight from Postgres.
func readComment(t *testing.T, d *testutil.DB, id string) (found, resolved bool, resolvedBy *string) {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT resolved, resolved_by FROM page_comments WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("read comment: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&resolved, &resolvedBy); err != nil {
			t.Fatalf("scan comment: %v", err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read comment: %v", err)
	}
	return found, resolved, resolvedBy
}

func seedComment(t *testing.T, d *testutil.DB) (*comment.Store, string, string) {
	t.Helper()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	pageID := d.Page(t, ws, author, "Doc")
	s := comment.NewStore(d.Pool)
	c, err := s.Create(context.Background(), pageID, nil, author, "Author", "is this right?")
	if err != nil {
		t.Fatalf("seed comment: %v", err)
	}
	return s, c.ID, author
}

// ⚠ Resolve is the state the UI hangs a whole thread off. It returns nil whether it resolved a
// thread or nothing at all, because the caller is `_, err := s.pool.Exec(...); return err` and
// zero rows affected is not an error.
func TestResolve_ActuallyMarksTheRowResolved_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	s, id, author := seedComment(t, d)

	found, resolved, _ := readComment(t, d, id)
	if !found || resolved {
		t.Fatalf("precondition: found=%v resolved=%v, want found and NOT resolved — the subject "+
			"was not in the state this test measures a change out of", found, resolved)
	}

	if err := s.Resolve(ctx, id, author); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	found, resolved, by := readComment(t, d, id)
	if !found {
		t.Fatal("the comment row vanished during Resolve")
	}
	if !resolved {
		t.Fatal("resolved is still FALSE after a Resolve that returned no error — the thread the " +
			"reader was told is settled is still open to every other reader")
	}
	if by == nil || *by != author {
		t.Errorf("resolved_by = %v, want %q — the row must record WHO settled it", by, author)
	}
}

// ⚠ Unresolve is the inverse and needs its own test: a predicate that matches nothing looks
// identical to a correct no-op from the outside, and only reopening a thread that IS resolved can
// tell them apart.
func TestUnresolve_ActuallyClearsTheRow_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	s, id, author := seedComment(t, d)

	if err := s.Resolve(ctx, id, author); err != nil {
		t.Fatalf("seed the resolved state: %v", err)
	}
	if _, resolved, _ := readComment(t, d, id); !resolved {
		t.Fatal("precondition: the comment is not resolved, so an Unresolve below could not be " +
			"distinguished from doing nothing")
	}

	if err := s.Unresolve(ctx, id); err != nil {
		t.Fatalf("Unresolve: %v", err)
	}

	_, resolved, by := readComment(t, d, id)
	if resolved {
		t.Fatal("resolved is still TRUE after an Unresolve that returned no error — reopening a " +
			"settled thread silently did nothing")
	}
	if by != nil {
		t.Errorf("resolved_by = %q after Unresolve, want NULL — a reopened thread still names a "+
			"resolver", *by)
	}
}

// ⚠ THE DESTRUCTIVE ONE. Delete gates on "only the author can delete" and then returns whatever
// the Exec returned, which is nil for a DELETE that removed nothing. The sec_root_actor and
// sec_crosspage suites drive this route and assert the STATUS CODE — 200 either way.
func TestDelete_ActuallyRemovesTheComment_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	s, id, author := seedComment(t, d)

	if found, _, _ := readComment(t, d, id); !found {
		t.Fatal("precondition: no row for the seeded comment — any post-delete count would be " +
			"meaningless")
	}

	if err := s.Delete(ctx, id, author); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if found, _, _ := readComment(t, d, id); found {
		t.Fatal("the comment row is STILL THERE after a Delete that returned no error — the " +
			"author was told their comment was removed and it is still on the page for everyone " +
			"who loads it next")
	}
}
