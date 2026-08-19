package comment_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/testutil"
)

// `include_resolved` IS AN INCLUDE FLAG, NOT AN ONLY FLAG — PINNED HERE BECAUSE THE SPA READ IT AS
// THE OTHER ONE AND THE SERVER HAD NOTHING SAYING WHICH IT WAS.
//
// ListByPage builds one statement and the parameter only ever ADDS a predicate:
//
//	q := `SELECT ... FROM page_comments WHERE page_id = $1`
//	if !includeResolved { q += ` AND resolved = false` }
//
// so true means "open AND resolved" and false means "open". CommentsPanel drove that flag from a
// two-value tab (`useComments(spaceID, pageID, tab === "resolved")`) and rendered the answer whole,
// so its "Resolved" tab listed the open threads too — beside a count from GetStats, which IS
// exclusive. Fixed in the panel, which selects from the superset it asked for; see
// CommentsPanel.resolvedtab.test.tsx.
//
// ⚠ THIS TEST IS THE REASON THAT FIX CANNOT BE MOVED DOWN A LAYER. Narrowing ListByPage to "only
// resolved" would fix that one screen and make the parameter lie about its own name for every
// future caller; it reds here the moment anyone tries. It is a MUST-STAY-GREEN pin on a contract,
// not a guard on a defect — the defect it exists for lives in the frontend.
//
// ⚠ THE ROWS ARE READ THROUGH ListByPage AND THE PRECONDITION THROUGH RAW SQL. Asserting the seed
// with the same method under test would let "resolve never landed" read as "the filter works":
// a page whose second thread was never resolved returns the same two rows either way.
func TestListByPage_IncludeResolvedIsInclusiveNotExclusive_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	pageID := d.Page(t, ws, author, "Doc with one of each")
	s := comment.NewStore(d.Pool)

	openC, err := s.Create(ctx, pageID, nil, author, "Author", "still open")
	if err != nil {
		t.Fatalf("seed open comment: %v", err)
	}
	doneC, err := s.Create(ctx, pageID, nil, author, "Author", "already settled")
	if err != nil {
		t.Fatalf("seed resolved comment: %v", err)
	}
	if err := s.Resolve(ctx, doneC.ID, author); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// PRECONDITION, straight from Postgres: exactly one of the two rows is resolved. Without this
	// both arms below would agree for a reason that has nothing to do with the flag.
	var resolvedCount int
	if err := d.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM page_comments WHERE page_id = $1 AND resolved = true`, pageID,
	).Scan(&resolvedCount); err != nil {
		t.Fatalf("precondition read: %v", err)
	}
	if resolvedCount != 1 {
		t.Fatalf("[PRECONDITION] expected exactly 1 resolved row on the page, got %d", resolvedCount)
	}

	ids := func(includeResolved bool) map[string]bool {
		t.Helper()
		got, err := s.ListByPage(ctx, pageID, includeResolved)
		if err != nil {
			t.Fatalf("list(include_resolved=%v): %v", includeResolved, err)
		}
		out := map[string]bool{}
		for _, c := range got {
			out[c.ID] = true
		}
		return out
	}

	// The default arm is the exclusive one — this is the half the SPA's "Open" tab relies on.
	if got := ids(false); len(got) != 1 || !got[openC.ID] {
		t.Fatalf("[EXCLUDES-RESOLVED] include_resolved=false must return only the open thread; got %d rows %v", len(got), got)
	}

	// The set arm is INCLUSIVE — it returns the open thread as well, which is precisely what the
	// resolved tab was painting.
	got := ids(true)
	if !got[doneC.ID] {
		t.Fatalf("[INCLUDES-RESOLVED] include_resolved=true must return the resolved thread; got %v", got)
	}
	if !got[openC.ID] {
		t.Fatalf("[INCLUSIVE-NOT-EXCLUSIVE] include_resolved=true must ALSO return the open thread — "+
			"it is an include flag, not a filter. If this now fails, ListByPage was narrowed and "+
			"CommentsPanel's client-side selection is no longer the thing keeping the tabs honest; got %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("[INCLUSIVE-NOT-EXCLUSIVE] include_resolved=true must return both threads; got %d %v", len(got), got)
	}
}
