package page_test

// DELETING A PAGE MOVES ITS CHILDREN UP AND LEAVES THEM CLAIMING THE DEPTH THEY HAD UNDERNEATH
// IT — AND THE FIRST THING THAT READS THAT NUMBER BACK IS THE DEPTH CAP.
//
// `Store.Delete` re-parents the deleted page's children onto its own parent, so every one of
// them lands at exactly the depth the deleted page occupied. It did not say so in the column:
// `depth` stayed where it was, one too deep, and every page beneath them with it. The method's
// own comment is candid about it — "Depth is intentionally NOT recomputed for the entire subtree
// in Phase 1… A 'rebalance depth' helper can come later if anyone notices the off-by-one" — and
// `_ = depth // kept for future rebalance hook` is the hook, already holding the right number.
//
// ⚠ IT IS THE OPPOSITE DIRECTION FROM #119's, AND THAT IS WHY IT IS A DIFFERENT DEFECT. A move
// left pages claiming to be SHALLOWER than they were, which let Create build past the cap. This
// leaves them claiming to be DEEPER, which makes Create refuse work that is legal: the page is
// really at depth 2, reports 4, and a caller adding two levels under it is told "page: max depth
// 5 exceeded" for a chain that would have been five long. A refusal nobody can act on, produced
// by somebody else deleting a different document, is the user-facing half — see [CREATE-REFUSED].
//
// ⚠ AND IT COMPOUNDS. Each delete of an ancestor adds another level of error to everything
// beneath it, so a page whose grandparent and great-grandparent are both deleted reports two
// deeper than it is. [COMPOUNDS] drives exactly that.
//
// ⚠ THE RE-BASE MUST NOT TOUCH `updated_at` — [CLOCK-DELETE]. This is the same statement's neighbour:
// `reparent_staleness_realpg_test.go` exists because Delete's re-parent DID re-date every child
// and cleared their freshness clock, the fourth copy of that seam in this repo. A depth re-base
// bolted on beside it is the fifth chance to make the same mistake, in the same method, against
// the same rows.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
)

// ── [DEPTH-AFTER-DELETE] ────────────────────────────────────────────────────────────────────
// The children land where the deleted page was. The column must say so, and so must everything
// under them.
func TestDeletingAParent_RebasesTheChildrensDepth_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(2) // root(0), c1(1), c2(2)
	kid := f.create("kid", &c[2].ID)
	grandkid := f.create("grandkid", &kid.ID)

	if err := f.store.DeleteInWorkspaces(f.ctx, c[2].ID, []string{f.ws}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, tc := range []struct {
		name string
		id   string
		want int
	}{
		{"the re-parented child", kid.ID, 2},
		{"its own child", grandkid.ID, 3},
	} {
		if got := f.recordedDepthOf(tc.id); got != tc.want {
			t.Errorf("[DEPTH-AFTER-DELETE] %s records depth %d, want %d (true depth %d)",
				tc.name, got, tc.want, f.trueDepthOf(tc.id))
		}
	}
}

// ── [CREATE-REFUSED] ────────────────────────────────────────────────────────────────────────
// The user-facing half. A stale-deep page makes Create refuse a chain that fits.
func TestAfterADelete_CreateStillAllowsEveryLevelThatFits_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(2)
	kid := f.create("kid", &c[2].ID) // depth 3, becomes depth 2 when c2 goes

	if err := f.store.DeleteInWorkspaces(f.ctx, c[2].ID, []string{f.ws}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// kid is truly at depth 2, so three more levels fit under it (3, 4, 5).
	cur := kid
	for i := 1; i <= 3; i++ {
		p, err := f.store.Create(f.ctx, model.Page{
			SpaceID: f.space, WorkspaceID: f.ws, Title: fmt.Sprintf("under%d", i),
			CreatedBy: f.who, ParentID: &cur.ID,
		})
		if err != nil {
			t.Fatalf("[CREATE-REFUSED] level %d under a page truly at depth %d was refused with %v — it reports depth %d, and the cap was applied to that",
				i, f.trueDepthOf(cur.ID), err, f.recordedDepthOf(cur.ID))
		}
		cur = p
	}
	if got := f.trueDepthOf(cur.ID); got != page.MaxDepth {
		t.Errorf("[CREATE-REFUSED] the deepest page sits at true depth %d, want exactly %d", got, page.MaxDepth)
	}
	// …and the cap still bites one level further on, so this is not "the cap stopped working".
	if _, err := f.store.Create(f.ctx, model.Page{
		SpaceID: f.space, WorkspaceID: f.ws, Title: "one too far", CreatedBy: f.who, ParentID: &cur.ID,
	}); !errors.Is(err, page.ErrMaxDepth) {
		t.Errorf("[CREATE-REFUSED] the level past the cap returned %v, want ErrMaxDepth", err)
	}
}

// ── [COMPOUNDS] ─────────────────────────────────────────────────────────────────────────────
// Two deletes up the chain, two levels of error, on a page nobody touched.
func TestTwoDeletesUpTheChain_DoNotAccumulateDepthError_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(3) // root(0) … c3(3)
	leaf := f.create("leaf", &c[3].ID)

	if err := f.store.DeleteInWorkspaces(f.ctx, c[3].ID, []string{f.ws}); err != nil {
		t.Fatalf("delete c3: %v", err)
	}
	if err := f.store.DeleteInWorkspaces(f.ctx, c[2].ID, []string{f.ws}); err != nil {
		t.Fatalf("delete c2: %v", err)
	}
	if got, want := f.recordedDepthOf(leaf.ID), 2; got != want {
		t.Errorf("[COMPOUNDS] after two deletes above it the leaf records depth %d, want %d (true depth %d) — each delete added a level of error",
			got, want, f.trueDepthOf(leaf.ID))
	}
}

// ── [CLOCK-DELETE] ─────────────────────────────────────────────────────────────────────────────────
// The re-base writes rows belonging to pages nobody edited, in the same method that already
// cleared their freshness clock once.
func TestTheDeleteRebase_DoesNotResetTheStalenessClock_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(2)
	kid := f.create("kid", &c[2].ID)
	grandkid := f.create("grandkid", &kid.ID)

	old := time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC)
	if _, err := f.d.Pool.Exec(f.ctx,
		`UPDATE pages SET updated_at = $1 WHERE id = ANY($2)`, old, []string{kid.ID, grandkid.ID}); err != nil {
		t.Fatalf("[CLOCK-DELETE] seed: %v", err)
	}
	if err := f.store.DeleteInWorkspaces(f.ctx, c[2].ID, []string{f.ws}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, id := range []struct {
		name string
		id   string
	}{{"the re-parented child", kid.ID}, {"its own child", grandkid.ID}} {
		var after time.Time
		if err := f.d.Pool.QueryRow(f.ctx, `SELECT updated_at FROM pages WHERE id = $1`, id.id).Scan(&after); err != nil {
			t.Fatalf("[CLOCK-DELETE] read back: %v", err)
		}
		if !after.UTC().Equal(old) {
			t.Errorf("[CLOCK-DELETE] %s was re-dated by somebody else's delete: %s -> %s",
				id.name, old.Format(time.RFC3339), after.UTC().Format(time.RFC3339))
		}
	}
	if got := f.recordedDepthOf(grandkid.ID); got != 3 {
		t.Errorf("[CLOCK-DELETE] grandchild records depth %d, want 3 — the re-base has to happen, just not through updated_at", got)
	}
}

// ── [ROOTS] ─────────────────────────────────────────────────────────────────────────────────
// Deleting a ROOT leaves its children as roots. The number Delete already holds — the deleted
// page's own depth — is 0 there, so the same expression covers this case rather than a branch.
func TestDeletingARoot_LeavesItsChildrenAtDepthZero_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	root := f.create("root", nil)
	kid := f.create("kid", &root.ID)
	grandkid := f.create("grandkid", &kid.ID)

	if err := f.store.DeleteInWorkspaces(f.ctx, root.ID, []string{f.ws}); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	if p := f.parentOf(kid.ID); p != nil {
		t.Fatalf("[ROOTS] the child of a deleted root still has a parent: %q", *p)
	}
	if got := f.recordedDepthOf(kid.ID); got != 0 {
		t.Errorf("[ROOTS] the new root records depth %d, want 0", got)
	}
	if got := f.recordedDepthOf(grandkid.ID); got != 1 {
		t.Errorf("[ROOTS] its child records depth %d, want 1", got)
	}
}

// ── [NO-CHILDREN] ───────────────────────────────────────────────────────────────────────────
// The vacuity floor for the re-base: deleting a leaf touches nothing, and a re-base seeded with
// an empty set must not become a re-base of the whole table.
func TestDeletingALeaf_LeavesEveryOtherDepthAlone_RealPG(t *testing.T) {
	f := newDepthFixture(t)
	c := f.chain(3)
	leaf := f.create("leaf", &c[3].ID)

	before := map[string]int{}
	for _, p := range c {
		before[p.ID] = f.recordedDepthOf(p.ID)
	}
	if err := f.store.DeleteInWorkspaces(f.ctx, leaf.ID, []string{f.ws}); err != nil {
		t.Fatalf("delete leaf: %v", err)
	}
	for id, want := range before {
		if got := f.recordedDepthOf(id); got != want {
			t.Errorf("[NO-CHILDREN] deleting a childless leaf changed an unrelated page's depth: %d -> %d", want, got)
		}
	}
}

var _ = context.Background
