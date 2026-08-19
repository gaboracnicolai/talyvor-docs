package templatelib_test

// THE TEMPLATE DOOR INTO page.Store.Create WAS NOT IN docs_pages_created_total.
//
// The full account of the defect is in internal/page/pagescreated_metric_realpg_test.go.
// UseTemplate calls page.Store.Create on both of its paths (built-in and custom) and moved the
// counter by ZERO on either. "Create a page from a template" is one of the two ways a page is
// made in the product UI; the metric saw one of them.
//
// [TEMPLATE-REFUSED-NOT-COUNTED] is the negative direction: the view-tier refusal this route
// already enforces (tier_test.go) must not move the counter either. A create that is refused is
// not a page created, and an Inc placed anywhere but beside the INSERT can fail that while
// passing every +1 assertion.
//
// RED at a59c424 (before the Inc moved into the store): moved by 0, want 1.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/testutil"
)

func TestTemplateUse_CountsInPagesCreatedTotal_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	viewer := d.Member(t, W, "viewer@corp.com")
	editor := d.Member(t, W, "editor@corp.com")

	targetSpace, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "Target", Slug: "target-" + owner[len(owner)-6:], Private: true, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed target space: %v", err)
	}
	grant := func(subject string, lvl permission.AccessLevel) {
		if _, err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourceSpace, ResourceID: targetSpace.ID, SubjectType: "member",
			SubjectID: subject, Access: lvl, WorkspaceID: W, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(viewer, permission.AccessView)
	grant(editor, permission.AccessEdit)

	srcPage := d.Page(t, W, owner, "Source")
	tmpl, err := templatelib.NewStore(d.Pool, page.NewStore(d.Pool)).CreateFromPage(
		ctx, srcPage, W, owner, "My Template", "desc", templatelib.CatGeneral, []string{W})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}

	chain := tierChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	pagesIn := func() int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id=$1`, targetSpace.ID).Scan(&n); err != nil {
			t.Fatalf("count pages: %v", err)
		}
		return n
	}
	useURL := "/v1/workspaces/" + W + "/template-library/" + tmpl.ID + "/use"

	// [TEMPLATE-COUNTED]
	rowsBefore, metricBefore := pagesIn(), testutil.ScrapeCounter(t, "docs_pages_created_total")
	rr := do(tierReq(http.MethodPost, useURL, "editor@corp.com", `{"space_id":"`+targetSpace.ID+`"}`))
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		t.Fatalf("template-use: HTTP %d — %s", rr.Code, rr.Body.String())
	}
	if got, want := pagesIn(), rowsBefore+1; got != want {
		t.Fatalf("precondition: template-use left %d rows in the space, want %d", got, want)
	}
	if got := testutil.ScrapeCounter(t, "docs_pages_created_total") - metricBefore; got != 1 {
		t.Errorf("[TEMPLATE-COUNTED] docs_pages_created_total moved by %v after template-use landed "+
			"a page, want 1 — the template door into page.Store.Create is not in the metric", got)
	}

	// [TEMPLATE-REFUSED-NOT-COUNTED]
	rowsBefore, metricBefore = pagesIn(), testutil.ScrapeCounter(t, "docs_pages_created_total")
	rr = do(tierReq(http.MethodPost, useURL, "viewer@corp.com", `{"space_id":"`+targetSpace.ID+`"}`))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("precondition: the view-tier template-use answered %d, want 403 — this case needs "+
			"a refusal to be about anything: %s", rr.Code, rr.Body.String())
	}
	if got, want := pagesIn(), rowsBefore; got != want {
		t.Fatalf("precondition: the refused template-use landed a row (%d → %d)", want, got)
	}
	if got := testutil.ScrapeCounter(t, "docs_pages_created_total") - metricBefore; got != 0 {
		t.Errorf("[TEMPLATE-REFUSED-NOT-COUNTED] docs_pages_created_total moved by %v on a "+
			"template-use that was refused 403 and landed no row, want 0", got)
	}
}
