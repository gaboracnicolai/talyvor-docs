package trackintegration

import (
	"context"
	"errors"
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

// ListWorkspaceIDs: this fake predates the enumeration inversion and its cases are about roster
// reconciliation, not about WHICH workspaces are covered. Returning an error makes the syncer fall
// back to the content-derived store these tests already seed — so they keep testing exactly what
// they were written to test, rather than being quietly re-pointed at a new source.
func (f *fakeMemberSource) ListWorkspaceIDs(context.Context) ([]string, error) {
	return nil, errors.New("fake: enumeration not exercised by these cases")
}

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

// PARKED — THE COLD-START DEADLOCK. This test pins a KNOWN LIMITATION, not a property
// worth having: a workspace Track knows about but Docs holds no content for is never
// enumerated, so its roster is never pulled, so nobody can be a member of it, so nobody
// can create the content that would enumerate it. The full decision, its reopening
// condition and the fix shape live on DistinctWorkspaceIDs in internal/membership/store.go.
//
// It is deliberately written to FAIL the moment the deadlock is broken. Whoever breaks it
// will see this test go red; the fix is not finished until they DELETE this test and the
// note it points at. A limitation recorded only in prose rots silently — this one cannot.
func TestSyncMembers_CannotReachAWorkspaceWithNoContent(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	withContent := d.Workspace(t)
	d.Page(t, withContent, "author", "PageA")

	// A brand-new per-user workspace: Track holds its roster, Docs holds nothing in it yet.
	empty := d.Workspace(t)

	store := membership.NewStore(d.Pool)
	fake := &fakeMemberSource{configured: true, rosters: map[string][]membership.MemberRef{
		withContent: {{Email: "alice@corp.com", Role: "admin", MemberID: "m1"}},
		empty:       {{Email: "newjoiner@corp.com", Role: "owner", MemberID: "m9"}},
	}}
	NewSyncer(nil, nil, nil, "").WithMemberSync(fake, store).SyncMembers(ctx)

	// Track is never even ASKED about the empty workspace — enumeration comes from content.
	for _, ws := range fake.calls {
		if ws == empty {
			t.Fatal("deadlock broken: SyncMembers asked Track about a workspace with no content. " +
				"If that is now intended, delete this test AND the parked-decision note on " +
				"DistinctWorkspaceIDs in internal/membership/store.go")
		}
	}

	// ...so no roster lands, and AuthorizeWorkspace can never pass for it.
	count := func(wsID string) int {
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_members WHERE workspace_id=$1`, wsID).Scan(&n); err != nil {
			t.Fatalf("count members: %v", err)
		}
		return n
	}
	if n := count(empty); n != 0 {
		t.Fatalf("deadlock broken: %d roster rows landed for a workspace with no content — see above", n)
	}

	// The premise, checked rather than assumed: the CONTENTFUL workspace DID sync. Without
	// this, a zero above is equally consistent with "SyncMembers did nothing at all", and the
	// test would pass while proving nothing.
	if n := count(withContent); n != 1 {
		t.Fatalf("premise broken: the workspace WITH content synced %d rows, want 1 — this test proves nothing", n)
	}
}
