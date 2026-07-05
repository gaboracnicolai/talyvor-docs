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
