package approval_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/approval"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 2 — RequestApproval's pages.doc_status flip was gated SOLELY by the route's
// pageEnf.Require wiring. This proves the NEW in-method gate (RequestApprovalInWorkspaces) holds ON
// ITS OWN (store driven directly, no enforcer): a caller authorized only for workspace B cannot
// open a review on / flip the doc_status of a workspace-A page.
func TestApproval_RequestInWorkspaces_CrossTenant_GateHoldsWithoutEnforcer_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")
	pA := d.Page(t, wsA, alice, "A doc")

	store := approval.NewStore(d.Pool)
	ctx := context.Background()

	docStatus := func() string {
		var s *string
		_ = d.Pool.QueryRow(ctx, `SELECT doc_status FROM pages WHERE id=$1`, pA).Scan(&s)
		if s == nil {
			return ""
		}
		return *s
	}
	before := docStatus()

	// Cross-tenant [wsB] → ErrNotFound, and the page's doc_status is untouched.
	if _, err := store.RequestApprovalInWorkspaces(ctx, pA, wsA, alice, []string{alice}, "review", nil, []string{wsB}); !errors.Is(err, approval.ErrNotFound) {
		t.Errorf("cross-tenant RequestApprovalInWorkspaces = %v, want ErrNotFound", err)
	}
	if got := docStatus(); got != before {
		t.Fatalf("cross-tenant request flipped doc_status %q → %q on a foreign page", before, got)
	}

	// Owner [wsA] → succeeds and moves the page into review (legit behavior unchanged).
	if _, err := store.RequestApprovalInWorkspaces(ctx, pA, wsA, alice, []string{alice}, "review", nil, []string{wsA}); err != nil {
		t.Fatalf("owner RequestApprovalInWorkspaces: %v", err)
	}
	if got := docStatus(); got == before {
		t.Fatalf("owner request did not change doc_status (still %q)", got)
	}
}
