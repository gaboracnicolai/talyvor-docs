package trackintegration

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/testutil"
)

// costsync_test.go — the cost-per-doc loop, which had ZERO tests while member sync in the
// same struct had two files of them. Everything here runs against a real Postgres and the
// REAL page/pagelink stores; only Track itself is faked, because Track is the thing whose
// failure the money path has to survive.

// fakeCostSource stands in for Track on the money path. It records the (workspace, issue)
// pairs it was asked for — the workspace matters as much as the number, because the loop
// used to ask about every tenant's issues using ONE pinned workspace id.
type fakeCostSource struct {
	configured  bool
	costs       map[string]float64 // "<wsID>|<issueID>" → AI cost
	unreachable map[string]bool    // "<wsID>|<issueID>" → the fetch fails
	asked       []string           // every key looked up, in order
}

func costKey(wsID, issueID string) string { return wsID + "|" + issueID }

func (f *fakeCostSource) IsConfigured() bool { return f.configured }

func (f *fakeCostSource) IssueCost(_ context.Context, wsID, issueID string) (float64, error) {
	k := costKey(wsID, issueID)
	f.asked = append(f.asked, k)
	if f.unreachable[k] {
		return 0, errors.New("fake track: 503 service unavailable")
	}
	cost, ok := f.costs[k]
	if !ok {
		// A miss is a FAILURE, not a zero. Asking about the wrong tenant's issue is
		// exactly the bug under test, and it must not read as "that issue cost nothing".
		return 0, fmt.Errorf("fake track: 404 no such issue %q in workspace %q", issueID, wsID)
	}
	return cost, nil
}

// seedLink writes the embed link the cost loop walks (IssueIDsForPage filters on
// link_type='embed').
func seedLink(t *testing.T, links *pagelink.Store, pageID, wsID, issueID string) {
	t.Helper()
	if err := links.Upsert(context.Background(), pagelink.PageLink{
		PageID:      pageID,
		WorkspaceID: wsID,
		IssueID:     issueID,
		LinkType:    "embed",
		CreatedBy:   "author",
	}); err != nil {
		t.Fatalf("seed link %s→%s: %v", pageID, issueID, err)
	}
}

func storedCost(t *testing.T, d *testutil.DB, pageID string) float64 {
	t.Helper()
	var got float64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&got); err != nil {
		t.Fatalf("read ai_cost_usd for %s: %v", pageID, err)
	}
	return got
}

// ⚠ FAILURE 1 — TWO LOOPS IN ONE STRUCT DISAGREEING ABOUT HOW MANY TENANTS EXIST.
//
// SyncPageCosts was pinned to DOCS_DEFAULT_WORKSPACE while SyncMembers, a few lines below it,
// enumerated every workspace. So the flagship "this spec cost $342 to ship" number was correct
// for exactly one workspace and permanently absent for every other one — on a deployment that
// mints a workspace per identity at login, that is everybody but the first tenant.
func TestSyncPageCosts_CoversEveryWorkspace_NotJustThePinnedOne(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsPinned, wsOther := d.Workspace(t), d.Workspace(t)
	pagePinned := d.Page(t, wsPinned, "author", "Pinned")
	pageOther := d.Page(t, wsOther, "author", "Other")

	links := pagelink.NewStore(d.Pool)
	seedLink(t, links, pagePinned, wsPinned, "ISS-1")
	seedLink(t, links, pageOther, wsOther, "ISS-2")

	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(wsPinned, "ISS-1"): 12.50,
		costKey(wsOther, "ISS-2"):  7.25,
	}}
	// Track answers the enumeration — the production path, the same one member sync takes.
	trackWS := &fakeMemberSource{configured: true, wsIDs: []string{wsPinned, wsOther}}

	NewSyncer(costs, page.NewStore(d.Pool), links, wsPinned).
		WithMemberSync(trackWS, membership.NewStore(d.Pool)).
		SyncPageCosts(ctx)

	// The premise, checked rather than assumed: without this a failure below is equally
	// consistent with the loop being broken for every workspace.
	if got := storedCost(t, d, pagePinned); got != 12.50 {
		t.Fatalf("premise broken: the PINNED workspace's page has $%.2f, want $12.50", got)
	}
	if got := storedCost(t, d, pageOther); got != 7.25 {
		t.Fatalf("a second workspace's page has $%.2f, want $7.25 — the cost loop is still pinned "+
			"to one tenant while SyncMembers in the same struct enumerates all of them. Every "+
			"workspace but the default shows $0.00 for a number the product sells", got)
	}
}

// ⚠ THE TRAP INSIDE FAILURE 1. Enumerating workspaces is only half the fix: the issue fetch
// must be scoped to the PAGE's workspace too. Keeping the pinned id in the GetIssue call would
// ask Track about tenant B's issue under tenant A's workspace — a 404 the old code read as
// "$0.00", which is the write-down this whole change exists to stop, just relocated.
func TestSyncPageCosts_AsksTrackUnderThePagesOwnWorkspace(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsPinned, wsOther := d.Workspace(t), d.Workspace(t)
	d.Page(t, wsPinned, "author", "Pinned")
	pageOther := d.Page(t, wsOther, "author", "Other")

	links := pagelink.NewStore(d.Pool)
	seedLink(t, links, pageOther, wsOther, "ISS-2")

	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(wsOther, "ISS-2"): 7.25,
	}}
	trackWS := &fakeMemberSource{configured: true, wsIDs: []string{wsPinned, wsOther}}

	NewSyncer(costs, page.NewStore(d.Pool), links, wsPinned).
		WithMemberSync(trackWS, membership.NewStore(d.Pool)).
		SyncPageCosts(ctx)

	want := costKey(wsOther, "ISS-2")
	for _, k := range costs.asked {
		if k == want {
			return
		}
	}
	t.Fatalf("Track was asked for %v, never %q — the issue fetch is still scoped to the pinned "+
		"workspace, so every other tenant's issue 404s and its page is written down to $0.00",
		costs.asked, want)
}

// ⚠ FAILURE 2 — A PARTIAL TOTAL OVERWROTE A COMPLETE ONE. LIVE, NOT LATENT.
//
// `ref, _ := GetIssue(...)` swallowed every error "intentionally" — correct for the embed it
// was written for, where an unavailable issue should render a placeholder rather than fail the
// page. Reused on the money path it means an unreachable issue contributes $0 and the too-low
// total is written straight over the good one. A customer reconciling a page's cost against
// their invoice sees a number that was never true, and nothing on the page says so.
//
// The rule: a total that is not COMPLETE is never written. A Track blip must leave the previous
// total exactly where it was.
func TestSyncPageCosts_ATrackFailureLeavesThePreviousTotalIntact(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	pageID := d.Page(t, ws, "author", "Spec")

	links := pagelink.NewStore(d.Pool)
	seedLink(t, links, pageID, ws, "ISS-1")
	seedLink(t, links, pageID, ws, "ISS-2")

	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(ws, "ISS-1"): 10.00,
		costKey(ws, "ISS-2"): 5.00,
	}}
	trackWS := &fakeMemberSource{configured: true, wsIDs: []string{ws}}
	syncer := NewSyncer(costs, page.NewStore(d.Pool), links, ws).
		WithMemberSync(trackWS, membership.NewStore(d.Pool))

	// A clean pass lands the COMPLETE total.
	syncer.SyncPageCosts(ctx)
	if got := storedCost(t, d, pageID); got != 15.00 {
		t.Fatalf("premise broken: a clean sweep stored $%.2f, want $15.00", got)
	}

	// Now Track goes down for ONE of the two issues, mid-loop.
	costs.unreachable = map[string]bool{costKey(ws, "ISS-2"): true}
	syncer.SyncPageCosts(ctx)

	got := storedCost(t, d, pageID)
	if got == 10.00 {
		t.Fatalf("a Track blip wrote the page down from $15.00 to $%.2f — the unreachable issue "+
			"contributed $0 and the PARTIAL total overwrote the COMPLETE one. That number is on "+
			"an invoice a customer reconciles against", got)
	}
	if got != 15.00 {
		t.Fatalf("after a failed sweep the page holds $%.2f, want the previous $15.00 left "+
			"intact — an incomplete total must never be written at all", got)
	}
}

// The other half of fail-closed: refusing to write on failure must not become refusing to
// write. A page whose links are all gone genuinely costs $0.00, and zero linked issues is a
// COMPLETE total, not an unknown one.
func TestSyncPageCosts_APageWithNoLinksIsWrittenAsZero(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	pageID := d.Page(t, ws, "author", "Unlinked")

	links := pagelink.NewStore(d.Pool)
	seedLink(t, links, pageID, ws, "ISS-1")
	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(ws, "ISS-1"): 9.00,
	}}
	trackWS := &fakeMemberSource{configured: true, wsIDs: []string{ws}}
	syncer := NewSyncer(costs, page.NewStore(d.Pool), links, ws).
		WithMemberSync(trackWS, membership.NewStore(d.Pool))

	syncer.SyncPageCosts(ctx)
	if got := storedCost(t, d, pageID); got != 9.00 {
		t.Fatalf("premise broken: stored $%.2f, want $9.00", got)
	}

	if err := links.Delete(ctx, pageID, "ISS-1"); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	syncer.SyncPageCosts(ctx)

	if got := storedCost(t, d, pageID); got != 0 {
		t.Fatalf("an unlinked page still holds $%.2f, want $0.00 — no linked issues is a complete "+
			"answer, and fail-closed must not turn into never-write", got)
	}
}

// Unconfigured Track is a clean no-op, mirroring member sync: nothing fetched, nothing written.
func TestSyncPageCosts_UnconfiguredIsNoOp(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	pageID := d.Page(t, ws, "author", "P")
	links := pagelink.NewStore(d.Pool)
	seedLink(t, links, pageID, ws, "ISS-1")

	costs := &fakeCostSource{configured: false, costs: map[string]float64{
		costKey(ws, "ISS-1"): 4.00,
	}}
	trackWS := &fakeMemberSource{configured: true, wsIDs: []string{ws}}
	// Called directly: SyncPageCosts must self-guard on IsConfigured the way SyncMembers
	// self-guards on memberSyncOn, rather than relying on its one caller to hold the gate.
	NewSyncer(costs, page.NewStore(d.Pool), links, ws).
		WithMemberSync(trackWS, membership.NewStore(d.Pool)).
		SyncPageCosts(ctx)

	if len(costs.asked) != 0 {
		t.Fatalf("unconfigured cost sync asked Track %v, want no calls", costs.asked)
	}
	if got := storedCost(t, d, pageID); got != 0 {
		t.Fatalf("unconfigured cost sync wrote $%.2f, want the column untouched", got)
	}
}

// A deployment with cost sync configured but member sync NOT (no DOCS_TRACK_MEMBER_SYNC_SECRET)
// still covers every workspace — via Docs' own content, which is the right question for cost:
// a workspace with no pages has nothing to cost. What it must NOT do is call Track's
// member-sync-gated enumeration and then log "Track unreachable" on the refusal. Track is
// reachable; a secret is unset. A warning that cannot tell those apart is worse than none,
// because the day Track really is down it reads as the same routine noise.
func TestSyncPageCosts_WithoutMemberSyncEnumeratesFromContentWithoutCallingTrack(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsA, wsB := d.Workspace(t), d.Workspace(t)
	pageA, pageB := d.Page(t, wsA, "author", "A"), d.Page(t, wsB, "author", "B")

	links := pagelink.NewStore(d.Pool)
	seedLink(t, links, pageA, wsA, "ISS-A")
	seedLink(t, links, pageB, wsB, "ISS-B")

	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(wsA, "ISS-A"): 3.00,
		costKey(wsB, "ISS-B"): 4.00,
	}}
	memberSyncOff := &fakeMemberSource{configured: false}

	NewSyncer(costs, page.NewStore(d.Pool), links, wsA).
		WithMemberSync(memberSyncOff, membership.NewStore(d.Pool)).
		SyncPageCosts(ctx)

	if got := storedCost(t, d, pageA); got != 3.00 {
		t.Errorf("wsA page has $%.2f, want $3.00", got)
	}
	if got := storedCost(t, d, pageB); got != 4.00 {
		t.Errorf("wsB page has $%.2f, want $4.00 — content-derived enumeration must still cover "+
			"every workspace when member sync is off", got)
	}
	if memberSyncOff.enumCalls != 0 {
		t.Errorf("the cost loop called Track's member-sync-gated enumeration %d time(s) with the "+
			"member-sync secret unset — it is refused every tick, and the refusal is logged as "+
			"\"Track unreachable\" while Track is perfectly reachable", memberSyncOff.enumCalls)
	}
}
