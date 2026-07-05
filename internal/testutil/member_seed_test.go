package testutil_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// (c) the real Member() seed helper writes a workspace_members ROW (not a synthetic id),
// scoped to the workspace — so the SEC-4 red test can seed alice@/A + bob@/B and the
// resolver can distinguish them. FAILS today (Member returns a synthetic id, writes nothing).
func TestMemberSeed_WritesRealScopedRow(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsA, wsB := d.Workspace(t), d.Workspace(t)

	aliceID := d.Member(t, wsA, "alice@corp.com")
	d.Member(t, wsB, "bob@corp.com")
	if aliceID == "" {
		t.Fatal("Member() returned an empty id")
	}

	count := func(wsID, email string) int {
		var n int
		_ = d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_members WHERE workspace_id=$1 AND email=$2`, wsID, email).Scan(&n)
		return n
	}
	if count(wsA, "alice@corp.com") != 1 {
		t.Fatal("Member() did not persist a workspace_members row for alice@/A")
	}
	if count(wsA, "bob@corp.com") != 0 {
		t.Fatal("bob@ (workspace B) leaked into workspace A's roster")
	}
	if count(wsB, "bob@corp.com") != 1 {
		t.Fatal("Member() did not persist bob@/B")
	}
}
