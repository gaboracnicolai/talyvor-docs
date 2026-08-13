package page_test

// A PAGE COULD BE ITS OWN ANCESTOR.
//
// `parent_id` is in Update's allowlist and #117 gave it a tenancy gate — the new parent must be
// a page of the same workspace. A cycle passes that gate by construction: every page in a cycle
// is in its own workspace. Measured at e9f6109 and still true at db1c349:
//
//	PATCH {"parent_id": <the page itself>}   -> 200, parent_id = id on disk
//	Y.parent = X, then X.parent = Y          -> 200, a two-page ring
//
// ⚠ WHAT THIS IS AND IS NOT, because the honest size of it is the point. NO WALKER IN THIS
// REPOSITORY LOOPS ON IT TODAY: internal/export gathers one level (`*p.ParentID == root.ID`),
// internal/mcp's `byParent` builds a one-level map, and Create reads the parent exactly once.
// So this is a data-integrity defect, not a hang — a ring of pages that belongs to no tree, is
// unreachable from any root, and renders nowhere. What makes it worth a merge rather than a note
// is that `MaxDepth` and the whole `depth` derivation are written as if the parent chain
// terminates, and the FIRST recursive read anybody adds inherits the ring.
//
// ⚠ THE GUARD ITSELF MUST NOT HANG ON DATA THAT PREDATES IT. A `WITH RECURSIVE ... UNION ALL`
// walk that reaches a ring never terminates; `UNION` does, because the recursive term stops when
// it produces no NEW id and a ring repeats its own on the second lap. Rings written before this
// guard are still on disk — it prevents new ones, it does not repair old ones. [NO-HANG] seeds
// one with raw SQL and drives the ONE operation that can make the walk meet it: re-parenting a
// ring member, which is exactly the move that repairs a ring.
//
// ⚠ CREATE NEEDS NO SUCH CHECK AND THAT IS STRUCTURAL, NOT AN OMISSION. A page being inserted
// has no id yet and no children, so its parent cannot be itself and cannot be its descendant.
// Said here because #117's finding was that a fix at one door is half a fix, and the reader
// should be able to tell "the other door is safe by shape" from "the other door was forgotten".

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func TestParentCannotBeSelfOrDescendant_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	pages := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")

	root := d.Page(t, ws, alice, "Root")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id = $1`, root).Scan(&spaceID); err != nil {
		t.Fatalf("read space: %v", err)
	}
	mk := func(title string, parent *string) string {
		t.Helper()
		p, err := pages.Create(ctx, model.Page{
			SpaceID: spaceID, WorkspaceID: ws, Title: title, ParentID: parent, CreatedBy: alice,
		})
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		return p.ID
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
	move := func(child, parent string) error {
		t.Helper()
		_, err := pages.UpdateInWorkspaces(ctx, child, map[string]any{"parent_id": parent}, []string{ws})
		return err
	}

	// A -> B -> C, a real three-generation chain, plus an unrelated page to move onto.
	a := mk("A", nil)
	b := mk("B", &a)
	c := mk("C", &b)
	bystander := mk("Bystander", nil)

	// PRECONDITION — the chain landed. Without it every refusal below is satisfied by a fixture
	// in which nothing is anybody's ancestor.
	if parentOf(b) != a || parentOf(c) != b {
		t.Fatalf("fixture is wrong: B.parent=%q (want A) C.parent=%q (want B)", parentOf(b), parentOf(c))
	}

	// [SELF] the one-length cycle.
	if err := move(a, a); err == nil {
		t.Errorf("[SELF] a page was accepted as its own parent")
	}
	if got := parentOf(a); got != "" {
		t.Errorf("[SELF] the self-link was written: parent_id = %q", got)
	}

	// [TWO-STEP] A -> B, then B -> A closes a ring. B is A's CHILD, so this is the shortest
	// cycle a self-check cannot see.
	if err := move(a, b); err == nil {
		t.Errorf("[TWO-STEP] a page was accepted as the child of its own child")
	}
	if got := parentOf(a); got != "" {
		t.Errorf("[TWO-STEP] the ring was written: A.parent_id = %q", got)
	}

	// [DEEP] the grandchild. A one-generation check would let this through, and it is the case
	// that says the walk is recursive rather than two levels of hand-rolled lookup.
	if err := move(a, c); err == nil {
		t.Errorf("[DEEP] a page was accepted as the child of its own grandchild")
	}
	if got := parentOf(a); got != "" {
		t.Errorf("[DEEP] the deep ring was written: A.parent_id = %q", got)
	}

	// ── the vacuity floor ────────────────────────────────────────────────────────────────────
	// [MOVE-STILL-WORKS] every assertion above is satisfied by a gate that refuses every parent.
	if err := move(a, bystander); err != nil {
		t.Errorf("[MOVE-STILL-WORKS] an ordinary move onto an unrelated page was refused: %v", err)
	}
	if got := parentOf(a); got != bystander {
		t.Errorf("[MOVE-STILL-WORKS] A.parent_id = %q, want %q", got, bystander)
	}
	// [UPWARD-MOVE] moving C up to sit beside B, under A. A is C's ANCESTOR, and the direction
	// that matters is which way the link points: a check that refused any relative — rather than
	// any DESCENDANT — would red here and nowhere else.
	if err := move(c, a); err != nil {
		t.Errorf("[UPWARD-MOVE] re-parenting a page onto its own grandparent was refused: %v", err)
	}
	if got := parentOf(c); got != a {
		t.Errorf("[UPWARD-MOVE] C.parent_id = %q, want %q", got, a)
	}

	// [NO-HANG] — A RING SEEDED BEHIND THE GUARD WITH RAW SQL, AND THE PAGE BEING MOVED IS IN IT.
	//
	// ⚠ THE FIRST VERSION OF THIS CASE MOVED AN UNRELATED PAGE AND PROVED NOTHING, which the
	// control harness showed by turning UNION into UNION ALL and watching the file stay green.
	// The reason is structural and worth writing down: a page has exactly ONE parent, so a ring
	// member's parent is another ring member — no edge can enter a ring from outside it. A ring
	// is therefore always a DISJOINT COMPONENT, and a downward walk from any page not in one can
	// never reach it. The only caller that can make this walk loop is somebody re-parenting a
	// ring member, which is precisely the operation that REPAIRS a ring: the guard must survive
	// the state it exists to prevent being re-entered from.
	ringX := mk("Ring X", nil)
	ringY := mk("Ring Y", &ringX)
	if _, err := d.Pool.Exec(ctx, `UPDATE pages SET parent_id = $1 WHERE id = $2`, ringY, ringX); err != nil {
		t.Fatalf("seed ring: %v", err)
	}
	if parentOf(ringX) != ringY || parentOf(ringY) != ringX {
		t.Fatalf("fixture is wrong: the seeded ring did not land")
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := pages.UpdateInWorkspaces(deadline, ringX, map[string]any{"parent_id": root}, []string{ws})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("[NO-HANG] a ring member could not be moved back out of the ring: %v", err)
		} else if got := parentOf(ringX); got != root {
			t.Errorf("[NO-HANG] the repair did not land: Ring X.parent_id = %q, want %q", got, root)
		}
	case <-time.After(12 * time.Second):
		t.Fatalf("[NO-HANG] the parent guard did not return within 12s when the moved page was in a ring")
	}
}
