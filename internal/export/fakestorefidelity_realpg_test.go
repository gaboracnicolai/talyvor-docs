package export

// THE DOUBLE THIS PACKAGE TESTS ITS CHILD EXPANSION AGAINST IGNORED EVERY PART OF THE STORE'S
// CONTRACT THAT THE EXPANSION DEPENDS ON — AND THAT IS WHY #134 WAS INVISIBLE FOR AS LONG AS IT
// WAS.
//
//	func (f *fakePages) List(_ context.Context, filter page.PageFilter) ([]model.Page, error) {
//	    return f.bySpace[filter.SpaceID], nil
//	}
//
// `page.Store.List` reads FOUR fields of the filter. The double read ONE. It had no LIMIT (the
// store defaults an unset one to 100 and clamps anything over 500), no OFFSET, and no parent
// scope. So `TestToMarkdown_IncludesChildrenInPositionOrder` — the only unit test of the child
// expansion in this package — was GREEN whether or not the shipped read truncated at 100 rows,
// because the fake it exercised had no 100 to hit. The finding needed real Postgres and 103 seeded
// pages to become visible; a double that answered the way its subject answers would have shown it
// with three.
//
// ⚠ AND AFTER #134 THE SAME GAP IS A HANG, NOT JUST A BLIND SPOT. `listChildren` now pages until a
// short batch comes back. A double that ignores `Offset` returns the same full slice on every
// iteration, so `len(batch) < childBatch` is never true and the loop never ends. Today's fixtures
// hold at most three pages, so nothing spins today — the hazard is entirely in what the next test
// to use this double will do, which is exactly the kind that is cheap now and expensive later.
//
// ⚠ THIS FILE PINS THE DOUBLE AGAINST THE REAL STORE RATHER THAN AGAINST A SECOND OPINION ABOUT
// THE STORE. Both are asked the SAME filters over the SAME rows and must return the same page ids
// in the same order. Rewriting the contract here in assertions is how a double drifts from its
// subject a second time — the point is to have ONE statement of the behaviour, in
// page.Store.List, and to make the fake answer to it.
//
// THE TAGS:
//
//	[PREMISE-DIFFERS]  the matrix contains filters whose answers are not all identical ← else agreement is trivial
//	[SAME-ROWS]        double and store return the same ids in the same order, per filter ← the defect
//	[TERMINATES]       paging over the double with >500 rows ends                        ← the hang after #134
//
// NOT PINNED, AND SAID SO RATHER THAN LEFT TO BE ASSUMED: the store's 500 CLAMP. Telling a double
// that clamps from one that does not needs 501 rows in the space, and no caller in this package
// asks for a limit above 500 — listChildren asks for exactly 500. Control E5 scores NOT CAUGHT for
// that reason, deliberately, instead of the matrix quietly implying the clamp is covered.

import (
	"context"
	"fmt"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// listMatrix is the set of filters the two implementations must agree on. It is written in terms
// of what a CALLER can ask for, not in terms of what either implementation happens to branch on.
func listMatrix(spaceID string, parent *string) []struct {
	name   string
	filter page.PageFilter
} {
	return []struct {
		name   string
		filter page.PageFilter
	}{
		{"unset limit (the store's default 100)", page.PageFilter{SpaceID: spaceID}},
		{"limit 5", page.PageFilter{SpaceID: spaceID, Limit: 5}},
		{"limit 5 offset 3", page.PageFilter{SpaceID: spaceID, Limit: 5, Offset: 3}},
		{"limit 5 offset past the end", page.PageFilter{SpaceID: spaceID, Limit: 5, Offset: 999}},
		{"limit above the store's clamp", page.PageFilter{SpaceID: spaceID, Limit: 9999}},
		{"negative limit (also the default)", page.PageFilter{SpaceID: spaceID, Limit: -1}},
		{"parent scope", page.PageFilter{SpaceID: spaceID, ParentID: parent}},
		{"parent scope + limit 1", page.PageFilter{SpaceID: spaceID, ParentID: parent, Limit: 1}},
		{"parent scope + limit 1 offset 1", page.PageFilter{SpaceID: spaceID, ParentID: parent, Limit: 1, Offset: 1}},
		{"a space that does not exist", page.PageFilter{SpaceID: "sp-nonexistent"}},
	}
}

func ids(ps []model.Page) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// brief renders an id list for a failure message. A hundred UUIDs on one line is a diff nobody
// reads; the COUNT and the first few are what tell a limit from an offset from a parent scope.
func brief(v []string) string {
	if len(v) <= 6 {
		return fmt.Sprintf("%d %v", len(v), v)
	}
	return fmt.Sprintf("%d %v …+%d more", len(v), v[:6], len(v)-6)
}

func TestFakePages_AnswersTheWayPageStoreAnswers_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "fidelity@example.com")

	store := page.NewStore(d.Pool)
	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, 'Fidelity', 'sp-fidelity', $2, false) RETURNING id`,
		ws, author).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}

	// A shape with enough structure that the filters in the matrix pull genuinely different
	// answers out of it: one root, four children of it, and more top-level pages than the store's
	// default limit.
	mk := func(title string, parent *string) *model.Page {
		p, err := store.Create(ctx, model.Page{
			SpaceID: spaceID, WorkspaceID: ws, Title: title, ParentID: parent, CreatedBy: author,
		})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return p
	}
	root := mk("Root", nil)
	for i := 0; i < 4; i++ {
		mk(fmt.Sprintf("Child %d", i), &root.ID)
	}
	// ⚠ MORE THAN 100 PAGES ON PURPOSE. With ten rows the "unset limit" and "negative limit" cases
	// are answered identically by a double that has no default at all, so a fake missing the
	// store's `limit <= 0 → 100` rule would agree with it and the matrix would score that rule as
	// covered. Control E4 is the one that says so. (The store's OTHER bound, the 500 clamp, is NOT
	// pinned here and this is the statement of that: discriminating it needs 501 rows, and no
	// caller in this package asks for a limit above 500 — listChildren asks for exactly 500.)
	for i := 0; i < 101; i++ {
		mk(fmt.Sprintf("Other %03d", i), nil)
	}

	// The double is loaded with what the store would return for the WHOLE space, which is exactly
	// how every fixture in this package builds it.
	whole, err := store.List(ctx, page.PageFilter{SpaceID: spaceID, Limit: 500})
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	fake := &fakePages{bySpace: map[string][]model.Page{spaceID: whole}}

	matrix := listMatrix(spaceID, &root.ID)

	// [PREMISE-DIFFERS]: if every filter in the matrix had the same answer, "the two agree" would
	// be a statement about nothing.
	seen := map[string]bool{}
	for _, c := range matrix {
		got, lErr := store.List(ctx, c.filter)
		if lErr != nil {
			t.Fatalf("store.List(%s): %v", c.name, lErr)
		}
		seen[fmt.Sprint(ids(got))] = true
	}
	if len(seen) < 6 {
		t.Fatalf("[PREMISE-DIFFERS] the %d filters in the matrix produce only %d distinct answers "+
			"from the real store — a matrix whose cases cannot be told apart cannot show that the "+
			"double reads the filter", len(matrix), len(seen))
	}

	for _, c := range matrix {
		want, wErr := store.List(ctx, c.filter)
		if wErr != nil {
			t.Fatalf("store.List(%s): %v", c.name, wErr)
		}
		got, gErr := fake.List(ctx, c.filter)
		if gErr != nil {
			t.Fatalf("fake.List(%s): %v", c.name, gErr)
		}
		w, g := ids(want), ids(got)
		if fmt.Sprint(w) != fmt.Sprint(g) {
			t.Errorf("[SAME-ROWS] %s:\n  page.Store.List → %s\n  fakePages.List  → %s\n"+
				"The double this package tests its child expansion against does not answer the way "+
				"the store it stands in for answers. That is not a cosmetic difference: it is why "+
				"TestToMarkdown_IncludesChildrenInPositionOrder stayed green through the 100-row "+
				"truncation #134 fixed — the fake had no limit to hit.",
				c.name, brief(w), brief(g))
		}
	}

	// [TERMINATES]: listChildren's loop shape, run against the double, over more rows than one
	// batch — but with a hard iteration cap instead of the real loop. Calling listChildren here
	// would be the honest thing only if a failure were survivable: against a double that ignores
	// Offset it never returns AND appends 500 rows per pass, so the red would be an OOM or a
	// 300-second CI timeout rather than a message. A guard whose failure mode is "the suite dies"
	// tells the next reader nothing.
	big := make([]model.Page, 0, childBatch+1)
	for i := 0; i <= childBatch; i++ {
		big = append(big, model.Page{
			ID: fmt.Sprintf("pg-big-%04d", i), SpaceID: "sp-big", WorkspaceID: "ws-1",
			ParentID: &root.ID,
		})
	}
	bigFake := &fakePages{bySpace: map[string][]model.Page{"sp-big": big}}

	const maxPasses = 10 // (childBatch+1)/childBatch = 2 passes is the true answer; 10 is slack.
	total, passes, terminated := 0, 0, false
	for offset := 0; passes < maxPasses; offset += childBatch {
		passes++
		batch, lErr := bigFake.List(ctx, page.PageFilter{
			SpaceID: "sp-big", ParentID: &root.ID, Limit: childBatch, Offset: offset,
		})
		if lErr != nil {
			t.Fatalf("[TERMINATES] fake.List: %v", lErr)
		}
		total += len(batch)
		if len(batch) < childBatch {
			terminated = true
			break
		}
	}
	if !terminated {
		t.Errorf("[TERMINATES] %d passes of listChildren's loop over the double never saw a short "+
			"batch, so the shipped loop would not end. A double that ignores Offset returns the "+
			"same full slice on every pass — after #134 that is not a blind spot, it is a hang, and "+
			"the only reason nothing spins today is that every fixture in this package holds three "+
			"pages or fewer.", maxPasses)
	} else if total != childBatch+1 {
		t.Errorf("[TERMINATES] the loop ended after collecting %d of %d rows", total, childBatch+1)
	}
}
