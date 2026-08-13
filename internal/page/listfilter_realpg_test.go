package page_test

// PageFilter.ParentID WAS A FIELD, NOT A FILTER.
//
// PageFilter's own comment says it "drives the List query". List's SQL named `space_id` and
// nothing else, so a caller that set ParentID got the whole space back and no error. The live
// caller is the MCP `list_pages` tool, whose schema promises the scope in words; see
// internal/mcp/listpages_parent_realpg_test.go for the surface half of this guard.
//
// This file is the STORE half, and it exists separately because List has four callers and only
// one of them is MCP: internal/page's own REST list, internal/export's sibling walk and
// cmd/docs' public-site adapter all reach the same query. A predicate that is wrong here is
// wrong for all of them, and a tool test cannot see three of the four.
//
// ⚠ BOTH DIRECTIONS, AND THE SECOND ONE IS THE LOAD-BEARING HALF. `WHERE false` satisfies every
// scoped assertion in this repository; [UNSCOPED] is the case that says a nil ParentID still
// lists the space, and it is the reason the fix cannot be "always filter".
//
// ⚠ WHAT IS DELIBERATELY NOT ASSERTED: a non-nil pointer to the EMPTY string. It means
// `parent_id = ''`, which matches nothing, and NOT "the pages with no parent" — the tempting
// reading. No caller constructs it (MCP only sets the field when the argument is non-empty), so
// pinning either meaning here would invent a contract instead of recording one. Said out loud so
// the next reader does not take the silence for an oversight.
//
// ⚠ WHAT [SPACE-STILL-HOLDS] DOES NOT EARN, printed here rather than left to be assumed. It is
// the sole speaker for NO mutation the control harness found. Dropping the parentheses around
// the parent clause — `space_id = $1 AND $4 IS NULL OR parent_id = $4`, precedence doing the
// damage — reds [SCOPED] as well, because the cross-space row is genuinely a child of the page
// [SCOPED] asks about. What the second space earns is a NAME: two assertions firing together say
// "the space scope stopped applying to scoped listings", where [SCOPED] alone would be read as
// "the parent scope is broken" and sent somebody to the wrong line. Controls:
// ~/talyvor-queue/w31-listparent-controls-7c4b.py, 10/10 as predicted after that prediction was
// corrected — the first version of it claimed sole-speakership and was wrong.

import (
	"context"
	"sort"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func TestList_ParentIDIsAPredicate_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pages := page.NewStore(d.Pool)

	anchor := d.Page(t, ws, alice, "Anchor")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id = $1`, anchor).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	// A SECOND space, with its own parent/child pair carrying the same shape. The space
	// predicate and the parent predicate must both hold: a fix that replaced one with the other
	// would still pass every single-space assertion.
	otherSpaceAnchor := d.Page(t, ws, alice, "Elsewhere Anchor")
	var otherSpaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id = $1`, otherSpaceAnchor).Scan(&otherSpaceID); err != nil {
		t.Fatalf("read second space: %v", err)
	}

	mk := func(space, title string, parent *string) string {
		t.Helper()
		p, err := pages.Create(ctx, model.Page{
			SpaceID: space, WorkspaceID: ws, Title: title, ParentID: parent, CreatedBy: alice,
		})
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		return p.ID
	}
	parent := mk(spaceID, "Parent", nil)
	mk(spaceID, "Child", &parent)
	mk(spaceID, "Not A Child", nil)
	// Same parent id, different space. Reachable in production: parent_id is in Update's
	// allowlist and carries no same-space check, so a page can be re-parented across spaces.
	mk(otherSpaceID, "Child In Another Space", &parent)

	list := func(t *testing.T, f page.PageFilter) []string {
		t.Helper()
		rows, err := pages.List(ctx, f)
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		out := make([]string, 0, len(rows))
		for _, p := range rows {
			out = append(out, p.Title)
		}
		sort.Strings(out)
		return out
	}
	same := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// PRECONDITION — the fixture landed in the space under test.
	all := []string{"Anchor", "Child", "Not A Child", "Parent"}
	if got := list(t, page.PageFilter{SpaceID: spaceID}); !same(got, all) {
		t.Fatalf("fixture is wrong: unfiltered List = %v, want %v", got, all)
	}

	// [SCOPED] — the children of the page asked about, and only those. Not the parent itself.
	if got, want := list(t, page.PageFilter{SpaceID: spaceID, ParentID: &parent}), []string{"Child"}; !same(got, want) {
		t.Errorf("[SCOPED] List(ParentID=Parent) = %v, want %v", got, want)
	}

	// [SPACE-STILL-HOLDS] — the cross-space child of the SAME parent belongs to the other space's
	// listing and to no other. Both predicates, not one.
	if got, want := list(t, page.PageFilter{SpaceID: otherSpaceID, ParentID: &parent}),
		[]string{"Child In Another Space"}; !same(got, want) {
		t.Errorf("[SPACE-STILL-HOLDS] List(space=other, ParentID=Parent) = %v, want %v", got, want)
	}

	// [UNSCOPED] — a nil ParentID lists the space, unfiltered. Asserted after the scoped calls.
	if got := list(t, page.PageFilter{SpaceID: spaceID}); !same(got, all) {
		t.Errorf("[UNSCOPED] List(no ParentID) = %v, want %v", got, all)
	}
}
