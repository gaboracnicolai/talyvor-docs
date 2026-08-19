package block_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/block"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 2 — block.Update/Delete were gated SOLELY by the route's blockEnf.Require wiring (fail-open
// before the enforcer fix). This proves the NEW in-method gate holds ON ITS OWN — driving the store
// directly, no enforcer in the picture: a caller authorized only for workspace B cannot update or
// delete a block whose page lives in workspace A. Mutation-proven (neuter assertInWorkspaces → the
// cross-tenant ops land).
func TestBlock_InWorkspaces_CrossTenant_GateHoldsWithoutEnforcer_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")
	pA := d.Page(t, wsA, alice, "A doc")

	store := block.NewStore(d.Pool)
	ctx := context.Background()
	blk, err := store.Create(ctx, model.Block{PageID: pA, Type: "paragraph", Content: `{"t":0}`, Position: 1})
	if err != nil {
		t.Fatalf("seed block: %v", err)
	}

	exists := func() bool {
		var ok bool
		_ = d.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blocks WHERE id=$1)`, blk.ID).Scan(&ok)
		return ok
	}

	// Cross-tenant [wsB] → ErrNotFound, and the block is untouched.
	if _, err := store.UpdateInWorkspaces(ctx, blk.ID, ptr(`{"t":9}`), ptr(2.0), []string{wsB}); !errors.Is(err, block.ErrNotFound) {
		t.Errorf("cross-tenant UpdateInWorkspaces = %v, want ErrNotFound", err)
	}
	if err := store.DeleteInWorkspaces(ctx, blk.ID, []string{wsB}); !errors.Is(err, block.ErrNotFound) {
		t.Errorf("cross-tenant DeleteInWorkspaces = %v, want ErrNotFound", err)
	}
	if !exists() {
		t.Fatal("cross-tenant op destroyed the block")
	}

	// Owner [wsA] → succeeds (legit behavior unchanged).
	if _, err := store.UpdateInWorkspaces(ctx, blk.ID, ptr(`{"t":1}`), ptr(3.0), []string{wsA}); err != nil {
		t.Fatalf("owner UpdateInWorkspaces: %v", err)
	}
	if err := store.DeleteInWorkspaces(ctx, blk.ID, []string{wsA}); err != nil {
		t.Fatalf("owner DeleteInWorkspaces: %v", err)
	}
	if exists() {
		t.Fatal("owner DeleteInWorkspaces did not delete")
	}
}

// ptr is the "this field WAS named" helper for Update's partial-update arguments — nil means the
// column is left alone (see block.Store.Update). Both call sites above name both fields, which is
// what keeps this file about the workspace gate and nothing else.
func ptr[T any](v T) *T { return &v }
