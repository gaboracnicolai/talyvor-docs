package mcp_test

// THE AGENT DOOR INTO page.Store.Create WAS NOT IN docs_pages_created_total.
//
// The full account of the defect is in internal/page/pagescreated_metric_realpg_test.go. This
// file holds the surface: `create_page`, the MCP tool an AI agent calls, drives the same
// page.Store.Create as the REST route and moved the counter by ZERO.
//
// ⚠ THIS IS THE SURFACE THE SIGNAL EXISTS FOR. Agent-authored pages are the traffic nobody is
// watching in a UI, so the scrape is the only place their rate is visible — and it reported
// them as not happening. A metric that under-reports the automated half of the traffic reads as
// a quiet service rather than a broken counter.
//
// ⚠ IT SCRAPES THE SHIPPED /metrics HANDLER rather than reading the counter variable — see
// testutil.ScrapeCounter. The number under test is the one an operator sees, so an unregistered
// or renamed counter fails here rather than passing on a variable nobody gathers.
//
// RED at a59c424 (before the Inc moved into the store): moved by 0, want 1.

import (
	"context"
	"net/http"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

func TestMCPCreatePage_CountsInPagesCreatedTotal_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	bob := d.Member(t, ws, "bob@corp.com")
	anchor := d.Page(t, ws, bob, "Anchor")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id = $1`, anchor).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	chain := newMCPChain(t, d)

	before := testutil.ScrapeCounter(t, "docs_pages_created_total")
	rr := callTool(chain, "bob@corp.com", true, "create_page", map[string]any{
		"space_id": spaceID, "title": "Agent Page", "content": "# hi",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create_page: HTTP %d — %s", rr.Code, rr.Body.String())
	}
	// PRECONDITION: the tool really did create a page. Without this a chokepoint refusal (which
	// answers 200 with a JSON-RPC error body) would satisfy "the counter did not move" for the
	// wrong reason once the fix lands.
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE title = 'Agent Page'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("precondition: create_page landed %d rows, want 1 — %s", n, rr.Body.String())
	}
	if got := testutil.ScrapeCounter(t, "docs_pages_created_total") - before; got != 1 {
		t.Errorf("[MCP-COUNTED] docs_pages_created_total moved by %v after the create_page tool "+
			"landed a page, want 1 — the agent door into page.Store.Create is not in the metric", got)
	}
}
