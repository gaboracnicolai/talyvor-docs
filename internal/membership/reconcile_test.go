package membership_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/testutil"
)

func seedWM(t *testing.T, d *testutil.DB, wsID, email, role, memberID string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO workspace_members (workspace_id, email, role, member_id) VALUES ($1,$2,$3,$4)`,
		wsID, email, role, memberID); err != nil {
		t.Fatalf("seed workspace_member: %v", err)
	}
}

func roster(t *testing.T, d *testutil.DB, wsID string) map[string]string { // email -> role
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT email, role FROM workspace_members WHERE workspace_id=$1`, wsID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var e, r string
		_ = rows.Scan(&e, &r)
		out[e] = r
	}
	return out
}

// (a) reconcile: upsert present + prune departed, SCOPED to the one workspace — another
// workspace's rows are never touched.
func TestReconcileWorkspace_UpsertAndPrune_Scoped(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsA, wsB := d.Workspace(t), d.Workspace(t)
	// initial: A = {alice(admin), dave(member)}, B = {bob(member)}
	seedWM(t, d, wsA, "alice@corp.com", "admin", "m1")
	seedWM(t, d, wsA, "dave@corp.com", "member", "m2")
	seedWM(t, d, wsB, "bob@corp.com", "member", "m3")

	s := membership.NewStore(d.Pool)
	// pull for A = {alice (role now member), carol (new)} → dave departs.
	up, pr, err := s.ReconcileWorkspace(ctx, wsA, []membership.MemberRef{
		{Email: "alice@corp.com", Role: "member", MemberID: "m1"},
		{Email: "carol@corp.com", Role: "member", MemberID: "m4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if up != 2 || pr != 1 {
		t.Fatalf("upserted=%d pruned=%d, want 2/1 (dave pruned)", up, pr)
	}
	a := roster(t, d, wsA)
	if a["alice@corp.com"] != "member" {
		t.Fatalf("alice role = %q, want member (updated)", a["alice@corp.com"])
	}
	if _, ok := a["carol@corp.com"]; !ok {
		t.Fatal("carol not inserted")
	}
	if _, ok := a["dave@corp.com"]; ok {
		t.Fatal("dave not pruned")
	}
	if len(a) != 2 {
		t.Fatalf("wsA roster size %d, want 2", len(a))
	}
	// B untouched (prune scoped to A only).
	b := roster(t, d, wsB)
	if len(b) != 1 || b["bob@corp.com"] != "member" {
		t.Fatalf("wsB roster disturbed by an A-scoped reconcile: %v", b)
	}
}

// (a2) EMPTY-PULL SAFETY: reconciling with an empty set must NOT prune the roster — a
// transient Track hiccup returning [] must not wipe every member.
func TestReconcileWorkspace_EmptyPull_DoesNotPrune(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	seedWM(t, d, ws, "alice@corp.com", "admin", "m1")

	s := membership.NewStore(d.Pool)
	up, pr, err := s.ReconcileWorkspace(ctx, ws, nil) // empty pull
	if err != nil {
		t.Fatal(err)
	}
	if up != 0 || pr != 0 {
		t.Fatalf("empty pull should touch nothing: upserted=%d pruned=%d", up, pr)
	}
	if r := roster(t, d, ws); len(r) != 1 || r["alice@corp.com"] != "admin" {
		t.Fatalf("EMPTY PULL WIPED THE ROSTER (footgun): %v", r)
	}
}

// ─── Provenance: the prune must refuse rows it did not sync ──────────────────
//
// WHY THIS EXISTS. Docs' tenancy currently has exactly one source: this syncer,
// pulling rosters from Track. That makes Docs unsellable without Track — an
// audit finding, not a design decision — and the option to give Docs its own
// tenancy root is deliberately being kept open.
//
// Keeping it open depends on a row this syncer did not create surviving a
// reconcile. Today it does, but only BY COINCIDENCE: a workspace Track has never
// heard of pulls an empty roster and hits the empty-pull guard, which exists to
// survive a bad fetch and says nothing about provenance. Inside a MIXED
// workspace there is no protection at all — a Docs-native member's email is not
// in Track's roster, so the prune deletes them.
//
// A coincidence is not a guarantee, and the person who relaxes empty-pull
// handling later will have no way to know it was load-bearing. These tests make
// the protection explicit and failable.

func seedWMSource(t *testing.T, d *testutil.DB, wsID, email, role, memberID, source string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO workspace_members (workspace_id, email, role, member_id, source) VALUES ($1,$2,$3,$4,$5)`,
		wsID, email, role, memberID, source); err != nil {
		t.Fatalf("seed workspace_member(source=%s): %v", source, err)
	}
}

func sourceOf(t *testing.T, d *testutil.DB, wsID, email string) (string, bool) {
	t.Helper()
	var src string
	err := d.Pool.QueryRow(context.Background(),
		`SELECT source FROM workspace_members WHERE workspace_id=$1 AND email=$2`, wsID, email).Scan(&src)
	if err != nil {
		return "", false
	}
	return src, true
}

// TestReconcileWorkspace_NeverPrunesRowsItDidNotSync is the guard. A Docs-native
// member sits in the SAME workspace as Track-synced ones and is absent from
// Track's roster — the case the empty-pull guard does not cover at all.
func TestReconcileWorkspace_NeverPrunesRowsItDidNotSync(t *testing.T) {
	d := testutil.New(t)
	s := membership.NewStore(d.Pool)
	const ws = "ws-mixed"

	seedWMSource(t, d, ws, "synced-stays@example.com", "member", "m1", "track")
	seedWMSource(t, d, ws, "synced-departs@example.com", "member", "m2", "track")
	seedWMSource(t, d, ws, "docs-native@example.com", "owner", "d1", "docs")

	// Track's roster contains only the one member who stayed.
	if _, pruned, err := s.ReconcileWorkspace(context.Background(), ws, []membership.MemberRef{
		{Email: "synced-stays@example.com", Role: "member", MemberID: "m1"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	} else if pruned != 1 {
		t.Errorf("pruned = %d, want 1 (only the departed TRACK member)", pruned)
	}

	got := roster(t, d, ws)
	if _, ok := got["docs-native@example.com"]; !ok {
		t.Error("the Docs-native member was PRUNED — the syncer deleted a row it did not create, " +
			"which forecloses Docs ever having its own tenancy root")
	}
	if _, ok := got["synced-departs@example.com"]; ok {
		t.Error("a departed TRACK member survived — the guard must not break the feature it protects")
	}
	if _, ok := got["synced-stays@example.com"]; !ok {
		t.Error("a present Track member was pruned")
	}
}

// TestReconcileWorkspace_UpsertStampsTrackProvenance: rows this syncer writes must
// be marked as its own, or the prune has nothing to distinguish and the guard above
// would pass vacuously.
func TestReconcileWorkspace_UpsertStampsTrackProvenance(t *testing.T) {
	d := testutil.New(t)
	s := membership.NewStore(d.Pool)
	const ws = "ws-stamp"

	if _, _, err := s.ReconcileWorkspace(context.Background(), ws, []membership.MemberRef{
		{Email: "fresh@example.com", Role: "member", MemberID: "m9"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if src, ok := sourceOf(t, d, ws, "fresh@example.com"); !ok || src != "track" {
		t.Errorf("synced row source = %q (found=%v), want \"track\"", src, ok)
	}
}

// TestReconcileWorkspace_DoesNotStealProvenanceOnConflict: if Track later reports an
// email that already exists as a Docs-native row, the upsert must not silently
// re-label it — that would hand the row to the syncer and make it prunable next pass.
func TestReconcileWorkspace_DoesNotStealProvenanceOnConflict(t *testing.T) {
	d := testutil.New(t)
	s := membership.NewStore(d.Pool)
	const ws = "ws-conflict"

	seedWMSource(t, d, ws, "both@example.com", "owner", "d1", "docs")
	if _, _, err := s.ReconcileWorkspace(context.Background(), ws, []membership.MemberRef{
		{Email: "both@example.com", Role: "member", MemberID: "m1"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if src, _ := sourceOf(t, d, ws, "both@example.com"); src != "docs" {
		t.Errorf("provenance became %q — a Docs-native row was re-labelled as track-synced "+
			"and is now prunable", src)
	}
}
