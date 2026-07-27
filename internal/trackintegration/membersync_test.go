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
	// wsIDs, when non-nil, makes ListWorkspaceIDs SUCCEED and return it — i.e. exercise the
	// production enumeration path (Track answers, no fallback). Left nil by the cases that are
	// about roster reconciliation; see the note on ListWorkspaceIDs.
	wsIDs []string
}

func (f *fakeMemberSource) MemberSyncConfigured() bool { return f.configured }

// ListWorkspaceIDs. With wsIDs nil this returns an error, which makes the syncer fall back to the
// content-derived store the roster-reconciliation cases already seed — so those keep testing what
// they were written to test rather than being quietly re-pointed at a new source.
//
// ⚠ THAT DEFAULT ONCE NEUTRALISED AN EXPIRY, AND THE wsIDs FIELD EXISTS SO IT CANNOT AGAIN.
// TestSyncMembers_CannotReachAWorkspaceWithNoContent existed for one purpose: to go RED the moment
// the cold-start deadlock was broken, so the fix could not land while the parked-decision note
// survived. The fix landed (c970329). The test kept PASSING — because this default routed it down
// the fallback, so it was asserting the old behaviour of a path production no longer takes. Two
// tests then asserted opposite things and both were green.
//
// A test used as an expiry must exercise the PRODUCTION path. Set wsIDs when the case is about
// which workspaces get covered.
func (f *fakeMemberSource) ListWorkspaceIDs(context.Context) ([]string, error) {
	if f.wsIDs != nil {
		return f.wsIDs, nil
	}
	return nil, errors.New("fake: enumeration not exercised by this case (set wsIDs to exercise it)")
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

// ⚠ INVERTED, NOT DELETED — AND THE INVERSION IS OVERDUE.
//
// This used to be TestSyncMembers_CannotReachAWorkspaceWithNoContent: a deliberate expiry, written
// to go RED the moment the cold-start deadlock was broken, with the instruction that the fix "is
// not finished until they DELETE this test and the note it points at".
//
// ⚠ THE DEADLOCK WAS BROKEN (c970329) AND THIS TEST KEPT PASSING. The fake's ListWorkspaceIDs
// returned an error by default, so the syncer fell back to content-derived enumeration and the
// test went on asserting the behaviour of a path production no longer takes. Meanwhile
// TestEnumerate_IncludesAWorkspaceWithNoContent asserted the opposite. Both were green, and the
// parked-decision note on DistinctWorkspaceIDs survived describing a defect that was already fixed.
//
// THE LESSON, recorded where it happened: an expiry implemented as a test must exercise the
// PRODUCTION path. A fake that stands in for the thing whose behaviour changed can be adjusted for
// locally good reasons by someone who has no idea a decision hangs off it.
//
// So this now pins the CURRENT contract, end to end and through the real enumeration: a workspace
// Track knows about but Docs holds no content for IS synced. If that stops being true, the
// cold-start deadlock is back and a brand-new tenant cannot create their first page.
func TestSyncMembers_ReachesAWorkspaceWithNoContent(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	withContent := d.Workspace(t)
	d.Page(t, withContent, "author", "PageA")

	// A brand-new per-user workspace: Track holds its roster, Docs holds nothing in it yet.
	empty := d.Workspace(t)

	store := membership.NewStore(d.Pool)
	fake := &fakeMemberSource{
		configured: true,
		// Track answers — the production path. Nil here is what neutralised the old expiry.
		wsIDs: []string{withContent, empty},
		rosters: map[string][]membership.MemberRef{
			withContent: {{Email: "alice@corp.com", Role: "admin", MemberID: "m1"}},
			empty:       {{Email: "newjoiner@corp.com", Role: "owner", MemberID: "m9"}},
		},
	}
	NewSyncer(nil, nil, nil, "").WithMemberSync(fake, store).SyncMembers(ctx)

	var asked bool
	for _, ws := range fake.calls {
		if ws == empty {
			asked = true
		}
	}
	if !asked {
		t.Fatal("SyncMembers never asked Track about a workspace with no content — the cold-start " +
			"deadlock is back: no roster lands, so its first write 403s forever. Enumeration must " +
			"come from Track (enumerate.go), not from Docs' own content.")
	}

	count := func(wsID string) int {
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_members WHERE workspace_id=$1`, wsID).Scan(&n); err != nil {
			t.Fatalf("count members: %v", err)
		}
		return n
	}
	if n := count(empty); n != 1 {
		t.Fatalf("the empty workspace synced %d roster rows, want 1 — a brand-new tenant still "+
			"cannot be authorized for their own workspace", n)
	}
	// The premise, checked rather than assumed: without this a 1 above is equally consistent with
	// a fixture that happened to write a row.
	if n := count(withContent); n != 1 {
		t.Fatalf("premise broken: the workspace WITH content synced %d rows, want 1", n)
	}
}
