package sharing

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// THE SHARE-LINK VIEW COUNTER COULD BE WRONG IN FOUR DIFFERENT WAYS AND THE WHOLE REPO STAYED
// GREEN. Measured on real Postgres before these tests existed
// (scripts/w31-viewcount-premeasure.py, run at 0c962ac): `go test ./...` passed repo-wide with
// Validate's `UPDATE share_links SET view_count = view_count + 1 WHERE id = $1` mutated to
// `+ 0` (the bump stops bumping), to `... AND FALSE` (the statement runs and matches no row),
// to `... OR TRUE` (ONE public view inflates EVERY share link in the table, across pages and
// across workspaces) and to `+ 2`. Only DELETING the statement outright reddened anything —
// that is #65's `ExpectationsWereMet` in newMockStore, and it is the one failure mode already
// held.
//
// ⚠ NO RED-FIRST MOMENT, AND THIS HEADER IS WHERE THAT IS SAID OUT LOUD. The behaviour is
// already correct; these tests pin it. That makes them worth exactly what their positive
// controls prove and no more — scripts/w31-sharelink-viewcount-controls.py is their ENTIRE
// justification, and it names one catcher per failure mode before it runs.
//
// ⚠ WHY A MOCK CANNOT REACH THIS CLASS, BY CONSTRUCTION: pgxmock never executes SQL. Every
// mutation above leaves the statement's matched prefix (`UPDATE share_links SET view_count`)
// and its single bound argument (link.ID) untouched, so `ExpectExec(...).WithArgs(...)` is
// satisfied by a statement that does the wrong thing to the database, or nothing at all.
// AND UNLIKE changelog.DeleteEntry THERE IS NO `RowsAffected() == 0` BRANCH TO RED THROUGH:
// Validate discards the Exec error ON PURPOSE (a counter must not fail a read), so a row read
// is the only thing left that can speak. That is why finding (11) ranked this write last and
// warned that an increment has no ` AND FALSE` analogue a mock can see.
//
// ⚠ THE COUNTER IS OBSERVABLE, WHICH IS WHY THIS IS A GUARD AND NOT A TRIPWIRE — and that was
// checked rather than assumed, because #66 found the neighbouring write (approval's publish)
// STRUCTURALLY INERT and its prescribed guard unable to fail. `cols` carries view_count,
// ListByPage returns it, and `GET /v1/spaces/{s}/pages/{p}/share` serialises it to the page's
// admins as `view_count`. The SPA declares it (frontend/src/api/sharing.ts) but does not paint
// it today; the API response is the live reader.

// seedLink inserts a share_links row with SQL and returns its id.
//
// Direct SQL, NOT Store.Create: the subject here is Validate's UPDATE, and seeding through the
// Store would let a broken Create disguise a broken Validate.
func seedLink(t *testing.T, d *testutil.DB, wsID, pageID, token string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO share_links (page_id, workspace_id, token, access, created_by)
		 VALUES ($1, $2, $3, 'view', 'u-seed') RETURNING id`,
		pageID, wsID, token,
	).Scan(&id); err != nil {
		t.Fatalf("seed share_link %q: %v", token, err)
	}
	return id
}

// viewCount reads the column with SQL AGAINST THE POOL — never through ListByPage or any other
// Store getter. An oracle that shares a code path with its subject is not an independent
// oracle: a Validate that never bumped and a ListByPage that mis-scanned would cancel out.
func viewCount(t *testing.T, d *testutil.DB, id string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT view_count FROM share_links WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read view_count for %s: %v", id, err)
	}
	return n
}

// TestValidate_BumpsTheViewedLinkByExactlyOne_RealPG holds the VALUE on the link that was
// actually viewed.
//
// It views the link TWICE on purpose. One view cannot tell `view_count = view_count + 1` from
// `view_count = 1` — both land on 1 from a fresh row — and an assignment would peg every link
// in the product at 1 view forever. The second view is the only thing that separates them.
//
// ⚠ IT IS BLIND TO THE BLAST RADIUS AND THAT IS BY DESIGN: under `WHERE id = $1 OR TRUE` this
// database holds exactly one link, so the target still reads 1 then 2 and this test passes.
// TestValidate_TouchesNoLinkButTheOneViewed_RealPG is that failure mode's only catcher.
func TestValidate_BumpsTheViewedLinkByExactlyOne_RealPG(t *testing.T) {
	d := testutil.New(t)
	store := NewStore(d.Pool)

	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	page := d.Page(t, ws, author, "Shared")
	id := seedLink(t, d, ws, page, "tok-exact")

	// PRECONDITION FIRST: a seed that never landed would make a stuck counter read as a
	// working one.
	if got := viewCount(t, d, id); got != 0 {
		t.Fatalf("precondition: a freshly seeded link has view_count = %d, want 0", got)
	}

	if _, err := store.Validate(context.Background(), "tok-exact", ""); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := viewCount(t, d, id); got != 1 {
		t.Errorf("after ONE public view, share_links.view_count = %d, want exactly 1 — the bump "+
			"either did not run, matched no row, or counted the wrong amount", got)
	}

	if _, err := store.Validate(context.Background(), "tok-exact", ""); err != nil {
		t.Fatalf("Validate (second view): %v", err)
	}
	if got := viewCount(t, d, id); got != 2 {
		t.Errorf("after TWO public views, share_links.view_count = %d, want exactly 2 — a count "+
			"stuck at 1 is an ASSIGNMENT wearing an increment's clothes", got)
	}
}

// TestValidate_TouchesNoLinkButTheOneViewed_RealPG holds the BLAST RADIUS: opening one public
// link must leave every other link's counter alone.
//
// Two bystanders, because there are two radii worth naming — a SIBLING link on the same page
// (both rows appear side by side in the same SharePanel list, so a wrong ordering between them
// is visible on one screen) and a link in ANOTHER WORKSPACE (a public, unauthenticated request
// writing to another tenant's row). `WHERE id = $1 OR TRUE` reaches both and is invisible to
// pgxmock: the statement prefix and the bound argument are unchanged.
//
// ⚠ IT DELIBERATELY ASSERTS NOTHING ABOUT THE VIEWED LINK'S COUNT. That is what makes each of
// these two tests earn its place: a mutation that stops the bump entirely leaves both
// bystanders at 0 and this test GREEN, and only the exact-value test above speaks. The stated
// consequence is that this test is one-directional — if the UPDATE vanished altogether it
// would pass on an untouched premise. That mode has two other catchers (the exact-value test,
// and newMockStore's ExpectationsWereMet), and control S5 is where both are shown to fire.
func TestValidate_TouchesNoLinkButTheOneViewed_RealPG(t *testing.T) {
	d := testutil.New(t)
	store := NewStore(d.Pool)

	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@example.com")
	pageA := d.Page(t, wsA, alice, "Alice's page")
	seedLink(t, d, wsA, pageA, "tok-viewed")
	sibling := seedLink(t, d, wsA, pageA, "tok-sibling")

	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@example.com")
	pageB := d.Page(t, wsB, bob, "Bob's page")
	foreign := seedLink(t, d, wsB, pageB, "tok-foreign")

	for _, b := range []struct {
		name string
		id   string
	}{{"same-page sibling", sibling}, {"other-workspace link", foreign}} {
		if got := viewCount(t, d, b.id); got != 0 {
			t.Fatalf("precondition: %s starts at view_count = %d, want 0", b.name, got)
		}
	}

	if _, err := store.Validate(context.Background(), "tok-viewed", ""); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if got := viewCount(t, d, sibling); got != 0 {
		t.Errorf("opening ONE share link bumped a SIBLING link on the SAME page: view_count = %d, "+
			"want 0 — both rows are listed together, so this is a wrong ordering on one screen", got)
	}
	if got := viewCount(t, d, foreign); got != 0 {
		t.Errorf("opening ONE workspace's share link bumped ANOTHER WORKSPACE's link: view_count = %d, "+
			"want 0 — an unauthenticated public read must not write to a foreign tenant's row", got)
	}
}
