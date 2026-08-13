package page_test

// docs_pages_created_total COUNTED ONE OF THE SIX DOORS INTO page.Store.Create.
//
// `metrics.PagesCreated` is the only page-creation signal this service exports, it is scraped
// from an UNAUTHENTICATED /metrics (cmd/docs/main.go mounts it outside the gatewayauth+authz
// boundary), and its Help string calls it "Pages created". The Inc() lived in
// page.Handler.Create — the REST door — and nowhere else, while `INSERT INTO pages` exists in
// exactly ONE place in this repository (page.Store.Create) with SIX production callers:
//
//	internal/page/handler.go      Create           REST  → counted
//	internal/mcp/server.go        toolCreatePage   MCP   → NOT counted
//	internal/templatelib/store.go UseTemplate ×2   REST  → NOT counted
//	internal/importer/importer.go Import ×3        REST  → NOT counted
//
// store.go's own comment above that INSERT already says "all SIX callers of Create reach it".
// So a Confluence import of five thousand pages, and every page an AI agent creates through the
// MCP tool, moved the counter by ZERO — and the shape of that failure is the worst one for a
// rate metric: it is not noisy or absent, it is a plausible small number, and the traffic it
// omits is exactly the bulk and automated traffic an operator watches this signal for.
//
// THE FIX IS THE SEAM, NOT SIX CALL SITES. The Inc moved to page.Store.Create, beside the one
// INSERT, so a seventh door counts by construction rather than by a reviewer remembering.
//
// THIS FILE IS THE MUST-STAY-GREEN HALF: it was GREEN before the change and has to stay green
// after it, because moving a counter is exactly the shape of edit that silently loses the case
// it already had. Its sibling guards — internal/mcp, internal/templatelib, internal/importer —
// were RED and are the finding.
//
// ⚠ [REFUSED-NOT-COUNTED] IS NOT PADDING, AND THE REFUSAL IT USES WAS CHOSEN RATHER THAN TAKEN.
// A counter bumped BEFORE the insert — at the top of Store.Create, or in the handler before the
// store call — satisfies every +1 assertion in all four files while counting creations that
// never happened, so the property needs a case where a create is REFUSED and no row lands.
//
// The obvious case is the wrong one: a caller the space ENFORCER refuses never reaches
// page.Handler.Create OR page.Store.Create, so the counter cannot move for any placement inside
// either — the assertion would be green by construction, a guard that cannot fail. The refusal
// used here is ErrParentNotFound: a parent_id naming a page in ANOTHER workspace. It travels the
// whole shipped route, enters Store.Create, and is refused by the parent lookup's workspace
// predicate BEFORE the INSERT. That is a refusal both candidate sites are upstream of.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

const pagesCreatedMetric = "docs_pages_created_total"

func TestPagesCreatedTotal_RESTCreateIsCounted_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	anchor := d.Page(t, ws, owner, "Anchor")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, anchor).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	// A page in ANOTHER workspace, used as a parent_id: Store.Create's parent lookup carries a
	// workspace predicate, so this is refused INSIDE the store, before the INSERT.
	wsOther := d.Workspace(t)
	otherOwner := d.Member(t, wsOther, "stranger@corp.com")
	foreignParent := d.Page(t, wsOther, otherOwner, "Foreign Parent")

	chain := createChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	pagesIn := func(spaceID string) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id=$1`, spaceID).Scan(&n); err != nil {
			t.Fatalf("count pages: %v", err)
		}
		return n
	}

	// [REST-COUNTED] — the door that always counted. Asserted through the shipped route so the
	// assertion survives the Inc moving out of the handler.
	rowsBefore, metricBefore := pagesIn(spaceID), testutil.ScrapeCounter(t, pagesCreatedMetric)
	rr := do(createReq("/v1/spaces/"+spaceID+"/pages", "owner@corp.com", `{"title":"Counted"}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("REST create: HTTP %d — %s", rr.Code, rr.Body.String())
	}
	if got, want := pagesIn(spaceID), rowsBefore+1; got != want {
		t.Fatalf("precondition: REST create left %d rows in the space, want %d", got, want)
	}
	if got := testutil.ScrapeCounter(t, pagesCreatedMetric) - metricBefore; got != 1 {
		t.Errorf("[REST-COUNTED] %s moved by %v after one REST create, want 1", pagesCreatedMetric, got)
	}

	// [REFUSED-NOT-COUNTED] — a create that reaches the store and is refused inside it is not a
	// page created. A row must land for the counter to move.
	rowsBefore, metricBefore = pagesIn(spaceID), testutil.ScrapeCounter(t, pagesCreatedMetric)
	rr = do(createReq("/v1/spaces/"+spaceID+"/pages", "owner@corp.com",
		`{"title":"Refused","parent_id":"`+foreignParent+`"}`))
	if rr.Code == http.StatusCreated {
		t.Fatalf("precondition: the cross-workspace parent_id create was ACCEPTED (HTTP %d) — this "+
			"case needs a refusal to be about anything: %s", rr.Code, rr.Body.String())
	}
	if got, want := pagesIn(spaceID), rowsBefore; got != want {
		t.Fatalf("precondition: the refused create landed a row (%d → %d)", want, got)
	}
	if got := testutil.ScrapeCounter(t, pagesCreatedMetric) - metricBefore; got != 0 {
		t.Errorf("[REFUSED-NOT-COUNTED] %s moved by %v on a create that entered Store.Create, was "+
			"refused (HTTP %d) and landed no row, want 0", pagesCreatedMetric, got, rr.Code)
	}
}
