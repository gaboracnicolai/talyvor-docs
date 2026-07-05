package trackintegration

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/testutil"
)

// fakeMemberSource injects per-workspace rosters without hitting real Track.
type fakeMemberSource struct {
	configured bool
	rosters    map[string][]membership.MemberRef
	calls      []string
}

func (f *fakeMemberSource) MemberSyncConfigured() bool { return f.configured }

func (f *fakeMemberSource) GetWorkspaceMembers(_ context.Context, wsID string) ([]membership.MemberRef, error) {
	f.calls = append(f.calls, wsID)
	return f.rosters[wsID], nil
}

// (b) MULTI-WORKSPACE: SyncMembers enumerates the distinct workspaces Docs holds content
// for and lands EACH roster — proving it is not single-workspace, and stays scoped.
func TestSyncMembers_MultiWorkspace(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsA, wsB := d.Workspace(t), d.Workspace(t)
	d.Page(t, wsA, "author", "PageA") // content in both → enumeration returns both
	d.Page(t, wsB, "author", "PageB")

	store := membership.NewStore(d.Pool)
	fake := &fakeMemberSource{configured: true, rosters: map[string][]membership.MemberRef{
		wsA: {{Email: "alice@corp.com", Role: "admin", MemberID: "m1"}},
		wsB: {{Email: "bob@corp.com", Role: "member", MemberID: "m2"}},
	}}
	NewSyncer(nil, nil, nil, "").WithMemberSync(fake, store).SyncMembers(ctx)

	count := func(wsID string) int {
		var n int
		_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1`, wsID).Scan(&n)
		return n
	}
	if count(wsA) != 1 || count(wsB) != 1 {
		t.Fatalf("multi-ws sync: wsA=%d wsB=%d, want 1/1 (both enumerated + synced)", count(wsA), count(wsB))
	}
	var leak int
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND email='bob@corp.com'`, wsA).Scan(&leak)
	if leak != 0 {
		t.Fatal("cross-workspace leak: bob@ (wsB) landed in wsA")
	}
}

// (d) ISOLATION: unset member-sync secret → SyncMembers is a clean no-op (nothing written,
// no fetch), mirroring cost-sync when unconfigured.
func TestSyncMembers_Unconfigured_NoOp(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	d.Page(t, ws, "author", "P")

	store := membership.NewStore(d.Pool)
	fake := &fakeMemberSource{configured: false, rosters: map[string][]membership.MemberRef{
		ws: {{Email: "x@corp.com", Role: "member", MemberID: "m"}},
	}}
	NewSyncer(nil, nil, nil, "").WithMemberSync(fake, store).SyncMembers(ctx)

	var n int
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM workspace_members`).Scan(&n)
	if n != 0 {
		t.Fatalf("unconfigured member-sync wrote %d rows, want 0 (no-op)", n)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unconfigured member-sync called GetWorkspaceMembers %d times, want 0", len(fake.calls))
	}
}
