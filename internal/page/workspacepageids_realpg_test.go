package page_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// workspacepageids_realpg_test.go — THE COST SWEEP'S ENUMERATOR IS THE ONLY THING THAT KEEPS ONE
// TENANT'S PAGES OUT OF ANOTHER TENANT'S PRICING PASS, AND NOTHING IN THIS REPOSITORY COULD SEE IT.
//
// MEASURED, not reasoned. scripts/w31-tenancypredicate-census-5r8k.py neuters one tenancy
// predicate at a time — the predicate stays in the statement and keeps referencing its
// placeholder, so the bind still type-checks, and becomes `(workspace_id = $1 OR TRUE)`, the
// inert-filter shape — and runs the WHOLE suite on a from-zero real Postgres. For
// `WorkspacePageIDs` the answer was SILENT: 0 packages red, 0 tests red, exit 0. The enumerator
// could return EVERY page in the database and this repository had no way to notice.
//
// WHAT THE PREDICATE HOLDS UP, from its one caller (trackintegration.Syncer.syncWorkspaceCosts):
//
//	pageIDs := WorkspacePageIDs(ctx, wsID)   ← the ONLY thing that says "these are wsID's pages"
//	for each pageID: pageTotal(ctx, wsID, pageID, …) → client.IssueCost(ctx, wsID, issueID)
//	                 UpdateAICost(ctx, pageID, total)
//
// The workspace is passed to Track SEPARATELY from the page. Nothing downstream re-checks that
// the page belongs to it — syncer.go says so itself ("Everything downstream is scoped to wsID"),
// and that sentence is true only because this predicate made it true. UpdateAICost is
// deliberately unscoped (`WHERE id = $2`, with a nosemgrep waiver whose stated justification is
// "the pageID is server-derived, never client-supplied" — server-derived BY THIS QUERY). So the
// blast radius of one missing conjunct is: every page in every workspace repriced from one
// tenant's Track workspace, written to a money column that GET /pages/{id}, PageView and the MCP
// get_page tool all serve. See sec_crosstenantcost_realpg_test.go in internal/trackintegration
// for that consequence end to end.
//
// [SCOPED]     the enumerator does not return another workspace's page
// [ENUMERATES] and it DOES return its own — without which [SCOPED] passes for a method that
//
//	returns nothing at all, which is the failure mode of every allowlist test
func TestWorkspacePageIDs_DoesNotEnumerateAnotherWorkspacesPages(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsMine := d.Workspace(t)
	wsTheirs := d.Workspace(t)
	minePage := d.Page(t, wsMine, "author", "Mine")
	theirsPage := d.Page(t, wsTheirs, "author", "Theirs")

	store := page.NewStore(d.Pool)
	got, err := store.WorkspacePageIDs(ctx, wsMine)
	if err != nil {
		t.Fatalf("WorkspacePageIDs(%s): %v", wsMine, err)
	}

	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}

	// [ENUMERATES] — the premise. A method that returned nil would satisfy [SCOPED] for a
	// reason that has nothing to do with tenancy.
	if !seen[minePage] {
		t.Fatalf("[ENUMERATES] premise broken: WorkspacePageIDs(%s) = %v, which does not include "+
			"its own page %s — the scope assertion below would pass for an enumerator that "+
			"returns nothing", wsMine, got, minePage)
	}

	if seen[theirsPage] {
		t.Fatalf("[SCOPED] WorkspacePageIDs(%s) returned %s, a page in workspace %s. This is the "+
			"cost sweep's page list: syncWorkspaceCosts then prices every id it gets under the "+
			"workspace it was ASKED about, so another tenant's document is valued from this "+
			"tenant's Track workspace and the number is written to pages.ai_cost_usd — served by "+
			"GET /pages/{id}, by PageView and by the MCP get_page tool. UpdateAICost is "+
			"deliberately unscoped on the grounds that its pageID is server-derived; this query "+
			"is what derives it", wsMine, theirsPage, wsTheirs)
	}
}
