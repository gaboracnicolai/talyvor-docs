package analytics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/testutil"
)

func viewCount(t *testing.T, d *testutil.DB, pageID string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), `SELECT view_count FROM pages WHERE id=$1`, pageID).Scan(&n); err != nil {
		t.Fatalf("read view_count: %v", err)
	}
	return n
}

func pageViewsCount(t *testing.T, d *testutil.DB, pageID string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM page_views WHERE page_id=$1`, pageID).Scan(&n); err != nil {
		t.Fatalf("count page_views: %v", err)
	}
	return n
}

// PHASE B — RecordView's page bump was gated SOLELY by the route enforcer (analyticsHandler
// .WithAccess(pageEnf) in main.go); one wiring edit away from a live cross-tenant write. This
// proves the NEW in-method store gate holds ON ITS OWN: driving the store directly — no route
// enforcer in the picture at all (the same shape the #32 sweep used for the other store gates)
// — a caller authorized only for workspace B cannot record a view on a workspace-A page.
//
// Mutation-proven RED: neuter RecordViewInWorkspaces' assertInWorkspaces check (call RecordView
// directly) → the cross-tenant view bumps the foreign page → the assertions below fail. GREEN
// with the gate in.
func TestRecordView_CrossTenant_GateHoldsWithoutEnforcer_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")
	pA := d.Page(t, wsA, alice, "A's doc")

	store := analytics.NewStore(d.Pool)
	ctx := context.Background()
	view := analytics.PageView{PageID: pA, WorkspaceID: wsA, ViewerID: alice, Duration: 10}

	// Cross-tenant caller (verified set = [wsB]) → ErrNotFound, and NO bump / NO page_views row.
	if err := store.RecordViewInWorkspaces(ctx, view, []string{wsB}); !errors.Is(err, analytics.ErrNotFound) {
		t.Errorf("cross-tenant RecordView = %v, want ErrNotFound (gate must hold in-method)", err)
	}
	if got := viewCount(t, d, pA); got != 0 {
		t.Errorf("cross-tenant RecordView bumped view_count to %d — must not touch a foreign page", got)
	}
	if got := pageViewsCount(t, d, pA); got != 0 {
		t.Errorf("cross-tenant RecordView inserted %d page_views rows — must not record on a foreign page", got)
	}

	// Owner (verified set = [wsA]) → succeeds, bumps exactly once + one page_views row. This is
	// the unchanged legitimate-caller behavior.
	if err := store.RecordViewInWorkspaces(ctx, view, []string{wsA}); err != nil {
		t.Fatalf("owner RecordView: %v", err)
	}
	if got := viewCount(t, d, pA); got != 1 {
		t.Errorf("owner RecordView → view_count %d, want 1", got)
	}
	if got := pageViewsCount(t, d, pA); got != 1 {
		t.Errorf("owner RecordView → %d page_views rows, want 1", got)
	}
}
