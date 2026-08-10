package changelog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/testutil"
)

// NOTHING IN THIS REPO ASSERTED THAT A DELETED CHANGELOG ENTRY IS GONE, OR THAT THE ENTRIES
// BESIDE IT SURVIVED.
//
// W3.1 finding (11), ranked first of the three remaining unheld writes because it is
// destructive and user-facing. `d9249c5` made the MOCK notice DeleteEntry's `DELETE FROM
// changelog_entries` CALL vanishing. A mock cannot reach further than that BY CONSTRUCTION:
// pgxmock never executes SQL, so a DELETE that is called with exactly the expected arguments
// and removes the WRONG ROWS satisfies every expectation in this package.
//
// ⚠ THE MEASUREMENT, NOT AN IMPRESSION. With `WHERE id = $1 AND workspace_id = ANY($2)`
// changed to `WHERE id = $1 OR workspace_id = ANY($2)` — one deletion of one entry now
// destroys EVERY changelog entry in the caller's workspaces — `go test ./...` against real
// Postgres was GREEN ACROSS THE WHOLE REPO: 451 pass, 0 fail, 0 skip, with 51 real-PG/SEC-4
// tests among them. Control D1 below is that mutation.
//
// ⚠ WHY SEC4_L2 DOES NOT COVER THIS, MEASURED AND NOT ASSUMED. internal/database/sec4_l2_test.go
// (d) deletes B's entry as Alice and asserts the row survives — but its 404 comes from the
// PERMISSION ENFORCER UPSTREAM, not from this store's own scoping, which is why `3c8fbb2`
// recorded that removing the store's workspace scope reddens nothing there. Test 3 below calls
// DeleteEntry DIRECTLY, so the store's own `workspace_id = ANY($2)` is what is under test.
//
// ⚠ THESE TESTS HAVE NO RED-FIRST MOMENT AND THAT IS SAID OUT LOUD. The product behaviour is
// already correct; they pin it. Nothing about them passing on the unmodified tree says they
// work — scripts/w31-changelog-delete-controls.py is their entire justification. 7/7 controls
// as predicted, store.go restored to its pristine sha256: D3 reds ONLY the blast-radius guard,
// D2 and D5 red ONLY the cross-tenant guard, and D6 (an unrelated edit in the same file) reds
// NONE of the three while the package's own mock test does.
//
// ⚠ AND THE ONE THAT IS NOT EARNED, RECORDED AS A LIMIT RATHER THAN QUIETLY KEPT. NO CONTROL
// REDS TestDeleteEntry_ActuallyRemovesTheRow_RealPG ALONE. Its row assertion is a strict SUBSET
// of the blast-radius guard's "the target entry survived" check, so nothing a one-line edit can
// do breaks the first without breaking the second. What it adds is a MINIMAL FIXTURE: under D4
// (` AND FALSE`) and D7 (`id != $1`) it reds through the ErrNotFound branch with the narrowest
// possible message, while the blast-radius guard reds through a different assertion on the same
// mutation. That is a readability argument, not a coverage one, and it is written down as such.
//
// ⚠ THE ROWS ARE READ WITH SQL AGAINST THE POOL, NEVER THROUGH GetEntry OR ListEntries: an
// oracle that shares a code path with its subject is not an independent oracle. Every test
// asserts its PRECONDITION first, so a seed that never landed cannot make "0 rows" read as a
// working delete.

// entryExists reads existence straight from Postgres — deliberately not GetEntry, which the
// same edit could break.
func entryExists(t *testing.T, d *testutil.DB, id string) bool {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM changelog_entries WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count changelog_entries: %v", err)
	}
	return n == 1
}

// seedEntry inserts one row directly and returns its id. Direct SQL rather than CreateEntry so
// the fixture cannot be moved by an edit to the code under test. published controls
// published_at, which control D3 keys on.
func seedEntry(t *testing.T, d *testutil.DB, wsID, pageID, version string, published bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO changelog_entries (page_id, workspace_id, version, title, published_at)
		 VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN NOW() ELSE NULL END) RETURNING id`,
		pageID, wsID, version, "Release "+version, published,
	).Scan(&id); err != nil {
		t.Fatalf("seed changelog entry %s: %v", version, err)
	}
	return id
}

// 1. THE ROW IS GONE. The narrowest claim, kept on its own so a failure names it.
func TestDeleteEntry_ActuallyRemovesTheRow_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	page := d.Page(t, ws, author, "Changelog")
	s := changelog.NewStore(d.Pool, nil)

	id := seedEntry(t, d, ws, page, "v1.0.0", false)
	if !entryExists(t, d, id) {
		t.Fatal("precondition: the seeded entry is not on disk — the subject never existed, so " +
			"the assertion below could not fail")
	}

	if err := s.DeleteEntry(ctx, id, []string{ws}); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}

	if entryExists(t, d, id) {
		t.Fatal("the entry is STILL on disk after a DeleteEntry that returned no error — the " +
			"release note the author believes is retracted is still published")
	}
}

// 2. NOTHING ELSE IS GONE. This is the assertion the mock cannot hold and the
// `RowsAffected() == 0 ⇒ ErrNotFound` branch cannot hold either: a DELETE that removes too
// much reports MORE rows affected, not zero, so the branch reads it as success.
func TestDeleteEntry_DeletesOnlyThatEntry_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	page := d.Page(t, ws, author, "Changelog")

	other := d.Workspace(t)
	otherAuthor := d.Member(t, other, "other@corp.com")
	otherPage := d.Page(t, other, otherAuthor, "Their changelog")

	s := changelog.NewStore(d.Pool, nil)

	target := seedEntry(t, d, ws, page, "v1.0.0", false)
	siblingPublished := seedEntry(t, d, ws, page, "v0.9.0", true)
	siblingDraft := seedEntry(t, d, ws, page, "v0.8.0", false)
	foreign := seedEntry(t, d, other, otherPage, "v3.0.0", true)

	for name, id := range map[string]string{
		"target": target, "published sibling": siblingPublished,
		"draft sibling": siblingDraft, "another workspace's entry": foreign,
	} {
		if !entryExists(t, d, id) {
			t.Fatalf("precondition: %s is not on disk — the fixture never landed, so the "+
				"survival assertions below could not fail", name)
		}
	}

	if err := s.DeleteEntry(ctx, target, []string{ws}); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}

	if entryExists(t, d, target) {
		t.Error("the target entry survived a DeleteEntry that returned no error")
	}
	if !entryExists(t, d, siblingPublished) {
		t.Error("deleting ONE entry destroyed the PUBLISHED entry beside it — every release " +
			"note in this workspace goes with the one the author meant to retract")
	}
	if !entryExists(t, d, siblingDraft) {
		t.Error("deleting ONE entry destroyed the DRAFT entry beside it")
	}
	if !entryExists(t, d, foreign) {
		t.Error("deleting ONE entry destroyed ANOTHER WORKSPACE'S entry — a cross-tenant " +
			"destructive write reached through a caller who was authorised for their own row")
	}
}

// 3. THE STORE'S OWN SCOPE REFUSES, AND LEAVES THE ROW. SEC4_L2 asserts this through the HTTP
// route, where the permission enforcer answers 404 before the store is reached; this calls the
// store directly, so `workspace_id = ANY($2)` is the only thing that can produce the refusal.
func TestDeleteEntry_RefusesAnotherWorkspacesEntryAndLeavesIt_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	_ = d.Member(t, wsA, "alice@corp.com")

	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pageB := d.Page(t, wsB, bob, "B's changelog")

	s := changelog.NewStore(d.Pool, nil)

	entB := seedEntry(t, d, wsB, pageB, "v2.0.0", false)
	if !entryExists(t, d, entB) {
		t.Fatal("precondition: B's entry is not on disk")
	}

	// Alice's verified workspace set is {A}. B's entry is not in it.
	err := s.DeleteEntry(ctx, entB, []string{wsA})
	if !errors.Is(err, changelog.ErrNotFound) {
		t.Errorf("DeleteEntry across tenants returned %v, want ErrNotFound — the store's own "+
			"workspace scope did not refuse", err)
	}
	if !entryExists(t, d, entB) {
		t.Error("B's changelog entry was DESTROYED by a caller who is only a member of A")
	}
}
