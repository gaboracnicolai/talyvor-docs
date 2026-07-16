package page_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 1 — VERSION TENANCY: get-one and compare are workspace-scoped off the SERVER-authorized
// workspace set. A caller who is not a member of the page's workspace must not read, diff, or
// otherwise observe its version history.
//
// This is the load-bearing guard. Proven RED by neutering the assertInWorkspaces gate (the
// cross-tenant reads then return the snapshot instead of ErrNotFound); GREEN with the gate in.
func TestVersions_GetAndCompare_CrossTenant_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com") // Bob belongs to B only

	pA := d.Page(t, wsA, alice, "A's roadmap")
	store := page.NewStore(d.Pool)
	ctx := context.Background()

	// Two committed saves → versions 1 and 2 on A.
	if _, err := store.Update(ctx, pA, map[string]any{"content": `{"rev":1}`, "updated_by": alice}); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if _, err := store.Update(ctx, pA, map[string]any{"content": `{"rev":2}`, "updated_by": alice}); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	// OWNER (wsA) can get-one and compare, and the version is self-describing (workspace_id=wsA).
	v1, err := store.GetVersionInWorkspaces(ctx, pA, 1, []string{wsA})
	if err != nil {
		t.Fatalf("owner get v1: %v", err)
	}
	if v1.Version != 1 || v1.WorkspaceID != wsA {
		t.Fatalf("owner v1 = {version:%d ws:%s}, want {1 %s}", v1.Version, v1.WorkspaceID, wsA)
	}
	from, to, err := store.CompareVersionsInWorkspaces(ctx, pA, 1, 2, []string{wsA})
	if err != nil {
		t.Fatalf("owner compare: %v", err)
	}
	if from.Version != 1 || to.Version != 2 {
		t.Fatalf("owner compare = (%d,%d), want (1,2)", from.Version, to.Version)
	}

	// CROSS-TENANT (wsB, not a member of A) must be rejected — ErrNotFound, no snapshot leaked.
	if _, err := store.GetVersionInWorkspaces(ctx, pA, 1, []string{wsB}); !errors.Is(err, page.ErrNotFound) {
		t.Errorf("cross-tenant get-one = %v, want ErrNotFound (no cross-tenant version read)", err)
	}
	if _, _, err := store.CompareVersionsInWorkspaces(ctx, pA, 1, 2, []string{wsB}); !errors.Is(err, page.ErrNotFound) {
		t.Errorf("cross-tenant compare = %v, want ErrNotFound (no cross-tenant diff)", err)
	}
}
