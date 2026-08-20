package trackintegration

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/testutil"
)

// sec_crosstenantcost_realpg_test.go — WHAT THE COST SWEEP DOES IF ITS PAGE LIST IS NOT SCOPED.
//
// The companion to internal/page/workspacepageids_realpg_test.go, which guards the predicate
// itself. This one asserts the CONSEQUENCE, because the predicate is not interesting and the
// money is: `syncWorkspaceCosts(wsID)` takes every id the enumerator hands it and prices it
// under wsID — the workspace travels to Track SEPARATELY from the page, and nothing downstream
// re-checks that the two belong together. A page from another tenant therefore gets a real
// number, computed from a workspace that does not own it, written to `pages.ai_cost_usd`.
//
// ⚠ THE FAKE MAKES A MISS AN ERROR, NOT A ZERO (see fakeCostSource.IssueCost), and that is
// exactly why this test seeds THE SAME issue id in both workspaces. Had the two pages embedded
// different issues, the wrong-tenant lookup would 404, pageTotal would return INCOMPLETE and
// nothing would be written — the defect would be invisible for a reason unrelated to tenancy.
// Two tenants embedding the same Track issue id is not exotic; ids are per-workspace.
//
// [FOREIGN-UNPRICED] the swept workspace does not price a page belonging to another workspace
// [OWN-PRICED]       its own page IS priced in the same pass — without this, [FOREIGN-UNPRICED]
//
//	passes whenever the sweep is broken for everything
//
// [FOREIGN-LINKED]   and the foreign page has an embed to price — without this it stays at 0
//
//	because there was nothing to compute, not because it was skipped
func TestSyncPageCosts_DoesNotPriceAnotherWorkspacesPages(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsMine := d.Workspace(t)
	wsTheirs := d.Workspace(t)
	mineSpace := seedSpace(t, d, wsMine)
	theirsSpace := seedSpace(t, d, wsTheirs)

	links := pagelink.NewStore(d.Pool)
	pages := page.NewStore(d.Pool).WithLinker(links)

	minePage := createPageWithEmbed(t, pages, mineSpace, wsMine, "Mine", "ISS-1", false)
	theirsPage := createPageWithEmbed(t, pages, theirsSpace, wsTheirs, "Theirs", "ISS-1", false)

	// [FOREIGN-LINKED] — the premise. If the other tenant's page had no embed, pageTotal would
	// compute $0.00 for it and "still 0" would say nothing about whether it was visited.
	if got := embedLinkCount(t, d, theirsPage); got != 1 {
		t.Fatalf("[FOREIGN-LINKED] premise broken: the other workspace's page has %d embed "+
			"links, want 1 — a page with nothing to price stays at 0 whether or not the sweep "+
			"enumerates it", got)
	}

	// Only wsMine is swept: no member sync is wired, so costWorkspaces falls back to the single
	// pinned workspace (syncer.go). wsTheirs is not part of this pass at all, and the correct
	// outcome for its page is that the sweep never touches it.
	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(wsMine, "ISS-1"): 12.34,
	}}
	NewSyncer(costs, pages, links, wsMine).SyncPageCosts(ctx)

	// [OWN-PRICED] — the control.
	if got := storedCost(t, d, minePage); got != 12.34 {
		t.Fatalf("[OWN-PRICED] premise broken: the swept workspace's own page has $%.2f, want "+
			"$12.34 — the sweep is not working at all here, so the assertion below is free", got)
	}

	if got := storedCost(t, d, theirsPage); got != 0 {
		t.Fatalf("[FOREIGN-UNPRICED] a page in workspace %s was priced at $%.2f by a sweep of "+
			"workspace %s. The only thing that decides which pages this pass touches is "+
			"page.WorkspacePageIDs' `WHERE workspace_id = $1`; every id it returns is priced "+
			"under the workspace the sweep was asked about, because syncWorkspaceCosts passes "+
			"wsID to Track and the page id to UpdateAICost as two independent values. The "+
			"result is one tenant's Track pricing written to another tenant's document and "+
			"served from GET /pages/{id}, PageView and the MCP get_page tool",
			wsTheirs, got, wsMine)
	}
}
