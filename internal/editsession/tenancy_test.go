package editsession_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/editsession"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 2 — TENANCY (load-bearing): every edit-session op scopes off the SERVER-authorized
// workspace set. A caller authorized only for workspace B cannot acquire, observe, heartbeat,
// take over, or release a session on a workspace-A page — each resolves to ErrNotFound (a 404
// with no cross-tenant existence oracle).
//
// Mutation-proven (see the run log): neutering pageWorkspace makes these cross-tenant ops
// succeed / observe the session; the gate restores ErrNotFound.
func TestEditSession_CrossTenant_NoAcquireObserveTakeover_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")

	pA := d.Page(t, wsA, alice, "A's doc")
	store := editsession.NewStore(d.Pool)
	ctx := context.Background()

	// Anchor: A's own member CAN acquire on A's page (denials below are scope, not a broken query).
	if _, err := store.Acquire(ctx, pA, []string{wsA}, alice); err != nil {
		t.Fatalf("owner acquire on own page: %v", err)
	}

	// Bob (member of B only) presenting his authorized set [wsB] must not touch A's session.
	bobWs := []string{wsB}
	if _, err := store.Get(ctx, pA, bobWs); !errors.Is(err, editsession.ErrNotFound) {
		t.Errorf("cross-tenant Get = %v, want ErrNotFound (no cross-tenant observe)", err)
	}
	if _, err := store.Acquire(ctx, pA, bobWs, bob); !errors.Is(err, editsession.ErrNotFound) {
		t.Errorf("cross-tenant Acquire = %v, want ErrNotFound (no cross-tenant acquire)", err)
	}
	if _, err := store.Takeover(ctx, pA, bobWs, bob); !errors.Is(err, editsession.ErrNotFound) {
		t.Errorf("cross-tenant Takeover = %v, want ErrNotFound (no cross-tenant takeover)", err)
	}
	if _, err := store.Heartbeat(ctx, pA, bobWs, bob); !errors.Is(err, editsession.ErrNotFound) {
		t.Errorf("cross-tenant Heartbeat = %v, want ErrNotFound", err)
	}
	if err := store.Release(ctx, pA, bobWs, bob); !errors.Is(err, editsession.ErrNotFound) {
		t.Errorf("cross-tenant Release = %v, want ErrNotFound", err)
	}

	// And A's session is intact + still Alice's (Bob's attempts had no effect).
	got, err := store.Get(ctx, pA, []string{wsA})
	if err != nil {
		t.Fatalf("owner Get after cross-tenant attempts: %v", err)
	}
	if got == nil || got.Holder != alice {
		t.Fatalf("A's session = %+v, want held by alice (unperturbed by cross-tenant ops)", got)
	}
}
