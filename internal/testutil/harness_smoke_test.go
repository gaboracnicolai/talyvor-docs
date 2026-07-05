package testutil_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// TestHarness_Smoke proves the substrate before any security test is built on it:
// New(t) stands up an isolated DB + applies the real schema (incl. the vector
// extension), the seed helpers persist a page in a workspace, and it round-trips
// by id. Teardown (t.Cleanup) drops the database. Skips cleanly without a real PG.
func TestHarness_Smoke_RoundTripsAPage(t *testing.T) {
	d := testutil.New(t)

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Roadmap")

	var gotWS, gotTitle, gotCreatedBy string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT workspace_id, title, created_by FROM pages WHERE id = $1`, pageID).
		Scan(&gotWS, &gotTitle, &gotCreatedBy); err != nil {
		t.Fatalf("read page back by id: %v", err)
	}
	if gotWS != ws {
		t.Fatalf("workspace_id = %q, want %q", gotWS, ws)
	}
	if gotTitle != "Roadmap" {
		t.Fatalf("title = %q, want Roadmap", gotTitle)
	}
	if gotCreatedBy != alice {
		t.Fatalf("created_by = %q, want %q", gotCreatedBy, alice)
	}
}
