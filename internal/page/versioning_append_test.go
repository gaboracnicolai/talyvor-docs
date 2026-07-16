package page_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 1 — APPEND-ONLY: version history is never truncated. The single-writer + versioning
// model requires that every committed save's snapshot survives — a restore point must not
// silently vanish because newer saves pushed it out of a rolling window.
//
// RED (pre-change): Update pruned page_versions to MaxVersionsPerPage (100). After 101 content
// saves, version 1 is DELETEd, so the assertions below fail. GREEN: the prune is removed, all
// 101 snapshots remain.
func TestVersions_AppendOnly_NeverTruncated_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	pageID := d.Page(t, ws, author, "Chatty page")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	const saves = 101 // one past the old MaxVersionsPerPage=100 window, so the prune would bite
	for i := 1; i <= saves; i++ {
		if _, err := store.Update(ctx, pageID, map[string]any{
			"content":    fmt.Sprintf(`{"type":"doc","rev":%d}`, i),
			"updated_by": author,
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}

	// Every save appended exactly one version: 101 saves → 101 versions, none dropped.
	var count int
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM page_versions WHERE page_id=$1`, pageID).Scan(&count); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if count != saves {
		t.Errorf("version count = %d, want %d — history was truncated (append-only violated)", count, saves)
	}

	// The oldest snapshot (version 1) must still be retrievable — it is the earliest restore
	// point and a rolling window is exactly what would have deleted it.
	var v1exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM page_versions WHERE page_id=$1 AND version=1)`, pageID,
	).Scan(&v1exists); err != nil {
		t.Fatalf("check v1: %v", err)
	}
	if !v1exists {
		t.Error("version 1 was pruned — the earliest restore point must never be truncated")
	}
}
