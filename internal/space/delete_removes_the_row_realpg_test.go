package space_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// NOTHING IN THIS REPO ASSERTED THAT A DELETED SPACE IS GONE.
//
// W3.1 finding (9), measured (scripts/w31-cross-package-write-controls.py): with
// `DELETE FROM spaces WHERE id = $1` removed from Store.Delete, `go test ./...` against real
// Postgres was GREEN across the WHOLE repo — including TestDelete_RemovesSpace, whose ExpectExec
// names that exact statement. `3c8fbb2` closed that half; the mock now reddens when the statement
// disappears.
//
// The half a mock cannot reach is a statement that RUNS and matches no row. Family P of that
// harness (` AND FALSE`) left the entire repo green, and this is `9c00a99`'s finding one container
// UP: a page the owner believes is gone is one document, a SPACE the owner believes is gone is
// every document in it, still on disk and still reachable by any query that does not filter on it.
//
// The SEC4 workspace-route tests do not cover this. They assert status codes on list, get and
// update; none of them deletes a space and then looks.

// countSpaces reads existence straight from Postgres — deliberately not GetBySlug or List, which
// are code the same edit could break.
func countSpaces(t *testing.T, d *testutil.DB, id string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM spaces WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count spaces: %v", err)
	}
	return n
}

func TestDelete_ActuallyRemovesTheSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	s := space.NewStore(d.Pool)

	sp, err := s.Create(ctx, model.Space{WorkspaceID: ws, Name: "Engineering", CreatedBy: owner})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	if n := countSpaces(t, d, sp.ID); n != 1 {
		t.Fatalf("precondition: %d rows for the seeded space, want 1 — the subject never existed, "+
			"so any post-delete count below would be meaningless", n)
	}

	if err := s.Delete(ctx, sp.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if n := countSpaces(t, d, sp.ID); n != 0 {
		t.Fatalf("%d rows for the space AFTER a Delete that returned no error, want 0. The space "+
			"the owner believes is gone is still on disk, and so is everything in it.", n)
	}
}

// ⚠ THE SAME QUESTION ONE LAYER UP — "the row is gone" and "the product says it is gone" are
// different claims and only the second is what a user sees. Kept separate so a failure names which
// of the two broke.
func TestDelete_TheDeletedSpaceIsNoLongerListed_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	s := space.NewStore(d.Pool)

	sp, err := s.Create(ctx, model.Space{WorkspaceID: ws, Name: "Engineering", CreatedBy: owner})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	before, err := s.List(ctx, ws)
	if err != nil {
		t.Fatalf("List before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("precondition: List returned %d spaces, want 1 — the subject was never listed, so "+
			"the assertion below could not fail", len(before))
	}

	if err := s.Delete(ctx, sp.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	after, err := s.List(ctx, ws)
	if err != nil {
		t.Fatalf("List after: %v", err)
	}
	for _, got := range after {
		if got.ID == sp.ID {
			t.Fatal("List STILL returns the space that was just deleted — the delete reported " +
				"success and the space is still on the sidebar")
		}
	}
}
