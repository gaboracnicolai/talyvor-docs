package block_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/block"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/testutil"
)

// A BLOCK'S PARENT IS A BLOCK ON THE SAME PAGE. Nothing said so.
//
// migrations/0002_pages.sql declares `parent_id TEXT REFERENCES blocks(id) ON DELETE CASCADE` —
// a foreign key to ANY block in the table, with no same-page and no same-workspace predicate — and
// block.Create took ParentID straight off the decoded request body (handler.go sets in.PageID from
// the URL and leaves ParentID alone). So `POST /v1/pages/{myPage}/blocks` with another tenant's
// block id as parent_id was ACCEPTED: the FK is satisfied because the row exists, and nothing else
// looked.
//
// Two consequences, the second of which this repo already defends against by name:
//
//	· A cross-tenant reference is stored in the creator's row, and the FK is ON DELETE CASCADE, so
//	  the lifetime of one workspace's block becomes a function of another workspace's deletions.
//	· A CROSS-TENANT EXISTENCE ORACLE over block ids: a well-formed id that exists somewhere
//	  returned 201, one that does not returned an FK error. store.go's own ErrNotFound comment
//	  states the invariant this violated — "Maps to 404 in the handler — no cross-tenant existence
//	  oracle."
//
// This surface has no caller yet (measured: no `/blocks` request path in talyvor-docs' SPA, nor in
// talyvor-suite, -lens, -track or -code; the page render path still reads pages.content), which is
// exactly why the hole survived — the routes are mounted and reachable but nothing exercises them.
type parentFixture struct {
	store        *block.Store
	pageA, pageB string
	victimBlock  string
}

func newParentFixture(t *testing.T) (*parentFixture, context.Context) {
	t.Helper()
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pageA := d.Page(t, wsA, alice, "attacker doc")
	pageB := d.Page(t, wsB, bob, "victim doc")

	store := block.NewStore(d.Pool)
	ctx := context.Background()
	victim, err := store.Create(ctx, model.Block{PageID: pageB, Type: "paragraph", Content: `{"t":0}`, Position: 1})
	if err != nil {
		t.Fatalf("seed victim block: %v", err)
	}
	return &parentFixture{store: store, pageA: pageA, pageB: pageB, victimBlock: victim.ID}, ctx
}

func strp(s string) *string { return &s }

// [CROSS-PAGE-PARENT] the finding. RED before the fix: this Create succeeded.
func TestBlockCreate_RefusesAParentOnAnotherPage_RealPG(t *testing.T) {
	f, ctx := newParentFixture(t)

	got, err := f.store.Create(ctx, model.Block{
		PageID:   f.pageA,
		Type:     "paragraph",
		Content:  `{"t":1}`,
		Position: 1,
		ParentID: strp(f.victimBlock),
	})
	if err == nil {
		t.Fatalf("[CROSS-PAGE-PARENT] Create ACCEPTED a parent_id owned by another page in another "+
			"workspace: stored block %s on page %s with parent %s (page %s). The FK to blocks(id) "+
			"is satisfied by any row in the table; only a same-page check can refuse this.",
			got.ID, f.pageA, f.victimBlock, f.pageB)
	}
}

// [ORACLE-CLOSED] the same refusal must not depend on the id existing. A parent that exists
// elsewhere and a parent that exists nowhere have to be refused the SAME way, or the error channel
// is itself the oracle: "FK violation" vs "accepted" distinguishes them, and so does any two
// distinct errors a caller can tell apart.
func TestBlockCreate_UnknownParentAndOtherPageParentAreIndistinguishable_RealPG(t *testing.T) {
	f, ctx := newParentFixture(t)

	mk := func(parent string) error {
		_, err := f.store.Create(ctx, model.Block{
			PageID: f.pageA, Type: "paragraph", Content: `{"t":1}`, Position: 1, ParentID: strp(parent),
		})
		return err
	}

	elsewhere := mk(f.victimBlock)
	nowhere := mk("00000000-0000-0000-0000-000000000000")
	if elsewhere == nil || nowhere == nil {
		t.Fatalf("[ORACLE-CLOSED] both must be refused; existing-elsewhere=%v nowhere=%v", elsewhere, nowhere)
	}
	if elsewhere.Error() != nowhere.Error() {
		t.Errorf("[ORACLE-CLOSED] a caller can tell the two apart, which is the existence oracle "+
			"restated in the error channel:\n  exists on another page: %v\n  exists nowhere:         %v",
			elsewhere, nowhere)
	}
}

// [SAME-PAGE-PARENT-WORKS] must-stay-green companion. A nesting fix that refuses every parent
// would pass the two tests above while breaking the only thing parent_id is for. This is the
// assertion that stops "reject all parents" from counting as a fix.
func TestBlockCreate_AcceptsAParentOnTheSamePage_RealPG(t *testing.T) {
	f, ctx := newParentFixture(t)

	root, err := f.store.Create(ctx, model.Block{
		PageID: f.pageA, Type: "paragraph", Content: `{"t":0}`, Position: 1,
	})
	if err != nil {
		t.Fatalf("seed root block on page A: %v", err)
	}
	child, err := f.store.Create(ctx, model.Block{
		PageID: f.pageA, Type: "paragraph", Content: `{"t":1}`, Position: 2, ParentID: strp(root.ID),
	})
	if err != nil {
		t.Fatalf("[SAME-PAGE-PARENT-WORKS] a parent on the SAME page must still be accepted: %v", err)
	}
	if child.ParentID == nil || *child.ParentID != root.ID {
		t.Errorf("[SAME-PAGE-PARENT-WORKS] parent not round-tripped: got %v want %s",
			child.ParentID, root.ID)
	}
}

// [NO-PARENT-WORKS] must-stay-green companion: the overwhelmingly common case, a top-level block
// with parent_id NULL, must not acquire a lookup that rejects it.
//
// SAID PLAINLY, BECAUSE THE CONTROL RUN MEASURED IT AND MY PREDICTION WAS WRONG: this one has NO
// isolating control. Dropping the `$5::text IS NULL` disjunct reddens FIVE tests in this package,
// not this one, because every fixture here seeds a parentless block first. The disjunct is
// load-bearing; this assertion is not the thing bearing it. It stays because it names the
// invariant at the point of failure — a five-test cascade tells you something broke, not what.
func TestBlockCreate_AcceptsNoParent_RealPG(t *testing.T) {
	f, ctx := newParentFixture(t)

	got, err := f.store.Create(ctx, model.Block{
		PageID: f.pageA, Type: "paragraph", Content: `{"t":0}`, Position: 1,
	})
	if err != nil {
		t.Fatalf("[NO-PARENT-WORKS] a block with no parent must still be accepted: %v", err)
	}
	if got.ParentID != nil {
		t.Errorf("[NO-PARENT-WORKS] parent_id should be NULL, got %v", *got.ParentID)
	}
}
