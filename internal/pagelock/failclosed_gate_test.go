package pagelock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 2 — pagelock.Lock/Unlock were gated SOLELY by the route's pageEnf.Require wiring. This
// proves the NEW in-method gate holds ON ITS OWN (store driven directly, no enforcer): a caller
// authorized only for workspace B cannot lock or unlock a workspace-A page.
func TestPagelock_InWorkspaces_CrossTenant_GateHoldsWithoutEnforcer_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pA := d.Page(t, wsA, alice, "A doc")

	store := pagelock.NewStore(d.Pool)
	ctx := context.Background()

	lockedBy := func() string {
		var by *string
		_ = d.Pool.QueryRow(ctx, `SELECT locked_by FROM pages WHERE id=$1`, pA).Scan(&by)
		if by == nil {
			return ""
		}
		return *by
	}

	// Cross-tenant [wsB] Lock → ErrNotFound, page stays unlocked.
	if _, err := store.LockInWorkspaces(ctx, pA, bob, []string{wsB}); !errors.Is(err, pagelock.ErrNotFound) {
		t.Errorf("cross-tenant LockInWorkspaces = %v, want ErrNotFound", err)
	}
	if lockedBy() != "" {
		t.Fatal("cross-tenant lock landed on a foreign page")
	}

	// Owner [wsA] Lock → succeeds.
	if _, err := store.LockInWorkspaces(ctx, pA, alice, []string{wsA}); err != nil {
		t.Fatalf("owner LockInWorkspaces: %v", err)
	}
	if lockedBy() != alice {
		t.Fatalf("owner lock: locked_by=%q, want alice", lockedBy())
	}

	// Cross-tenant [wsB] Unlock → ErrNotFound, Alice's lock survives.
	if err := store.UnlockInWorkspaces(ctx, pA, bob, false, []string{wsB}); !errors.Is(err, pagelock.ErrNotFound) {
		t.Errorf("cross-tenant UnlockInWorkspaces = %v, want ErrNotFound", err)
	}
	if lockedBy() != alice {
		t.Fatal("cross-tenant unlock cleared a foreign page's lock")
	}

	// Owner [wsA] Unlock → succeeds.
	if err := store.UnlockInWorkspaces(ctx, pA, alice, false, []string{wsA}); err != nil {
		t.Fatalf("owner UnlockInWorkspaces: %v", err)
	}
	if lockedBy() != "" {
		t.Fatal("owner unlock did not clear the lock")
	}
}
