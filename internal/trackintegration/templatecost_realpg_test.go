package trackintegration

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/testutil"
)

// templatecost_realpg_test.go — THE COST SWEEP'S ENUMERATOR DROPS TEMPLATES, AND A COLUMN THE
// SCHEMA DOCUMENTS AS "RECOMPUTED ON EVERY SWEEP" IS THEREFORE NEVER RECOMPUTED FOR THEM.
//
// `WorkspacePageIDs` ends `AND is_template = false`, and its comment gives the reason as "so the
// sync loop doesn't churn on un-released specs". Every OTHER site in this repository that
// mentions `is_template` is a READER — SearchWithRank, search/semantic ×2, analytics' never-read
// cohort, customdomain.Handler — and there the predicate removes a row from an ANSWER. This one
// is a WRITER, and there the same predicate does something else entirely: it leaves
// `pages.ai_cost_usd` on a row that is still served by id, still rendered by PageView, and still
// emitted by the MCP `get_page` tool.
//
// ⚠ THE CLAIM THAT IS FALSE IS IN THE SCHEMA, NOT IN A COMMENT SOMEBODY MIGHT NOT HAVE READ.
// migrations/0018 carries `COMMENT ON COLUMN pages.ai_cost_usd`: "DERIVED — recomputed from a
// complete set of links on every sweep and overwritten … Do not add to this column: the next
// sweep overwrites it." For a template there is no next sweep. That sentence is the reason the
// column was dropped from Create's INSERT and kept off Update's allowlist (store.go:320-331,
// sec_aicost_create_test.go) — two guards that hold the column shut on the grounds that the
// sweep owns it, over a cohort the sweep never visits.
//
// ⚠ AND THE COHORT IS ONE PATCH AWAY, NOT A FIXTURE. `is_template` IS in Update's allowlist
// (store.go:566, 779) — the same fact internal/analytics/templatecohort_realpg_test.go measured
// from the other end ("2 ids → 1" through this very enumerator). So a document that has been
// priced at $12.34 and is then marked as a template keeps $12.34 on screen forever, whatever
// happens to the issues it embeds.
//
// The two failures are ONE test each because they are two directions of one predicate:
//
//	[TEMPLATE-PRICED]   a template's linked-issue cost is computed, not left at the column
//	                    DEFAULT 0 — a fabricated zero, byte-identical on every surface to a
//	                    document that genuinely cost nothing
//	[TEMPLATE-REPRICED] a page marked as a template afterwards is still repriced — the number
//	                    does not freeze at whatever it happened to hold
//
// and each carries the premise it would otherwise be vacuous without:
//
//	[TEMPLATE-HAS-LINKS] the shipped Create path DOES write page_links rows for a template
//	                     (SyncLinks has no is_template guard), so there is something to price
//	[PLAIN-PRICED]       a plain page in the same workspace, same issue, same tick IS priced —
//	                     without this, "the sweep is broken for everything" reads as this defect

// embedDoc is a minimal ProseMirror document embedding one issue — the shape pagelink.ParseEmbeds
// walks (`type: "issue_embed"`, `attrs.issue_id`). Written out rather than fixture-loaded so the
// link premise below is about the SHIPPED parser and not about a seeded row.
func embedDoc(issueID string) string {
	return `{"type":"doc","content":[{"type":"issue_embed","attrs":{"issue_id":"` + issueID + `"}}]}`
}

// seedSpace persists one space and returns its id. testutil.DB.Page mints a space per page; these
// tests want two pages that differ ONLY in is_template, so they share one.
func seedSpace(t *testing.T, d *testutil.DB, wsID string) string {
	t.Helper()
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by)
		 VALUES ($1, 'Template cost', 'space-templatecost-'||md5(random()::text), 'author')
		 RETURNING id`, wsID).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return spaceID
}

// createPageWithEmbed goes through the SHIPPED page.Store.Create with the link hook wired, so the
// page_links rows under test are the ones production writes, not seeded ones.
func createPageWithEmbed(t *testing.T, pages *page.Store, spaceID, wsID, title, issueID string, isTemplate bool) string {
	t.Helper()
	out, err := pages.Create(context.Background(), model.Page{
		SpaceID:     spaceID,
		WorkspaceID: wsID,
		Title:       title,
		Content:     embedDoc(issueID),
		ContentText: title + " body",
		IsTemplate:  isTemplate,
		CreatedBy:   "author",
	})
	if err != nil {
		t.Fatalf("create %q (is_template=%v): %v", title, isTemplate, err)
	}
	return out.ID
}

func embedLinkCount(t *testing.T, d *testutil.DB, pageID string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM page_links WHERE page_id = $1 AND link_type = 'embed'`,
		pageID).Scan(&n); err != nil {
		t.Fatalf("count embed links for %s: %v", pageID, err)
	}
	return n
}

func TestSyncPageCosts_ATemplatesLinkedIssuesAreNotReportedAsZero(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	spaceID := seedSpace(t, d, ws)

	links := pagelink.NewStore(d.Pool)
	pages := page.NewStore(d.Pool).WithLinker(links)

	plainID := createPageWithEmbed(t, pages, spaceID, ws, "Runbook", "ISS-1", false)
	templateID := createPageWithEmbed(t, pages, spaceID, ws, "Runbook template", "ISS-1", true)

	// [TEMPLATE-HAS-LINKS] — the premise. SyncLinks runs on every content-bearing save with no
	// is_template guard (store.go:405), so a template accumulates exactly the rows the money
	// column is derived from. If this ever stops being true the two assertions below become
	// vacuous rather than wrong, which is why it is asserted and not assumed.
	if got := embedLinkCount(t, d, templateID); got != 1 {
		t.Fatalf("[TEMPLATE-HAS-LINKS] the template has %d embed links, want 1 — the shipped "+
			"Create path no longer records issue embeds for templates, so the cost assertions "+
			"below would pass for a reason that has nothing to do with the sweep", got)
	}

	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(ws, "ISS-1"): 12.34,
	}}
	NewSyncer(costs, pages, links, ws).SyncPageCosts(ctx)

	// [PLAIN-PRICED] — the control. A sweep that is broken for every page would satisfy
	// "the template is not priced" for the wrong reason.
	if got := storedCost(t, d, plainID); got != 12.34 {
		t.Fatalf("[PLAIN-PRICED] premise broken: the plain page embedding ISS-1 has $%.2f, "+
			"want $12.34 — the sweep is not working at all here", got)
	}

	// [TEMPLATE-IS-SERVED] — the other premise, and the one that makes the zero a LIE rather
	// than an absence. `GetByID` is `SELECT … FROM pages WHERE id = $1` with no template
	// predicate, so the row the sweep skipped is handed to PageView, to GET /pages/{id} and to
	// the MCP get_page tool exactly as any other page. Read back through that shipped path
	// rather than off the column, so the assertion is about what a caller receives.
	served, err := pages.GetByID(ctx, templateID)
	if err != nil {
		t.Fatalf("[TEMPLATE-IS-SERVED] premise broken: the shipped read path refused the "+
			"template (%v) — if templates were not served, an unmaintained column on them "+
			"would be invisible rather than wrong", err)
	}

	if got := served.AICostUSD; got != 12.34 {
		t.Fatalf("[TEMPLATE-PRICED] the template embedding ISS-1 reports $%.2f, want $12.34. "+
			"WorkspacePageIDs ends `AND is_template = false`, so the sweep never visits this "+
			"row and pages.ai_cost_usd keeps its column DEFAULT — a fabricated zero, "+
			"byte-identical on PageView, on GET /pages/{id} and in the MCP get_page tool to a "+
			"document that genuinely cost nothing. migrations/0018 COMMENTs this very column "+
			"as \"recomputed from a complete set of links on every sweep\"", got)
	}
}

func TestSyncPageCosts_APageMarkedAsATemplateIsStillRepriced(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	spaceID := seedSpace(t, d, ws)

	links := pagelink.NewStore(d.Pool)
	pages := page.NewStore(d.Pool).WithLinker(links)

	pageID := createPageWithEmbed(t, pages, spaceID, ws, "Launch spec", "ISS-9", false)

	costs := &fakeCostSource{configured: true, costs: map[string]float64{
		costKey(ws, "ISS-9"): 12.34,
	}}
	NewSyncer(costs, pages, links, ws).SyncPageCosts(ctx)
	if got := storedCost(t, d, pageID); got != 12.34 {
		t.Fatalf("[PLAIN-PRICED] premise broken: $%.2f before the flip, want $12.34", got)
	}

	// The live path, not a fixture: `is_template` is in Update's allowlist, so this is a PATCH a
	// user can send. Through the shipped Update so the allowlist, not a raw UPDATE, is what
	// moves the row into the cohort.
	if _, err := pages.Update(ctx, pageID, map[string]any{
		"is_template": true,
		"updated_by":  "author",
	}); err != nil {
		t.Fatalf("PATCH is_template=true: %v", err)
	}

	// Track reprices the issue — the sweep's whole reason to run on a timer.
	costs.costs[costKey(ws, "ISS-9")] = 3.00
	NewSyncer(costs, pages, links, ws).SyncPageCosts(ctx)

	if got := storedCost(t, d, pageID); got != 3.00 {
		t.Fatalf("[TEMPLATE-REPRICED] after the page was marked as a template the sweep left "+
			"$%.2f, want $3.00. Marking a document as boilerplate silently froze a money "+
			"figure that is still served by GET /pages/{id} and still rendered — and nothing "+
			"on the page, in the API or in the schema says the number stopped being maintained; "+
			"migrations/0018 says the opposite", got)
	}
}
