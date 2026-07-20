package templatelib_test

// A3 tier enforcement for template-USE. POST /v1/workspaces/{wsID}/template-library/{id}/use creates a
// page in a space named in the request BODY, gated (pre-fix) only by AuthorizeWorkspace(wsID) — never the
// target space's AccessEdit that page.Create enforces at the canonical door. So a view-only member could
// instantiate a template into a space they may only view. RED (handler ungated): the viewer's Use creates
// a page — assert on pages in the DB, not just the status. GREEN (spaceauth gate): viewer 403 + no page;
// editor succeeds; a space_id in another workspace is 404 (no oracle).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/testutil"
)

const tierSecret = "sec4-test-gateway-secret-0123456789"

func tierChain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	h := templatelib.NewHandler(templatelib.NewStore(d.Pool, page.NewStore(d.Pool))).
		WithAccess(spaceauth.New(spaceStore, permStore))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(tierSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func tierReq(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", tierSecret)
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

func TestA3_TemplateUse_TierEnforcement(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	viewer := d.Member(t, W, "viewer@corp.com")
	editor := d.Member(t, W, "editor@corp.com")

	// The TARGET space (where Use will create a page). Private, so tiers are explicit: viewer=view,
	// editor=edit; nobody gets an implicit default.
	targetSpace, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "Target", Slug: "target-" + owner[len(owner)-6:], Private: true, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed target space: %v", err)
	}
	grant := func(spaceID, subject string, lvl permission.AccessLevel) {
		if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourceSpace, ResourceID: spaceID, SubjectType: "member",
			SubjectID: subject, Access: lvl, WorkspaceID: W, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(targetSpace.ID, viewer, permission.AccessView)
	grant(targetSpace.ID, editor, permission.AccessEdit)

	// A source page + a custom template built from it (the thing being instantiated).
	srcPage := d.Page(t, W, owner, "Source")
	tmpl, err := templatelib.NewStore(d.Pool, page.NewStore(d.Pool)).CreateFromPage(
		ctx, srcPage, W, owner, "My Template", "desc", templatelib.CatGeneral, []string{W})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}

	// A space in a DIFFERENT workspace (for the cross-tenant 404 case).
	W2 := d.Workspace(t)
	other := d.Member(t, W2, "other@corp.com")
	foreignSpace, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W2, Name: "Foreign", Slug: "foreign-" + other[len(other)-6:], CreatedBy: other,
	})
	if err != nil {
		t.Fatalf("seed foreign space: %v", err)
	}

	chain := tierChain(d)
	code := func(r *http.Request) int { rr := httptest.NewRecorder(); chain.ServeHTTP(rr, r); return rr.Code }
	pagesIn := func(spaceID string) int {
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id=$1`, spaceID).Scan(&n); err != nil {
			t.Fatalf("count pages: %v", err)
		}
		return n
	}
	useURL := "/v1/workspaces/" + W + "/template-library/" + tmpl.ID + "/use"
	wrote := func(c int) bool { return c == http.StatusOK || c == http.StatusCreated }

	// ── (a) RED: a view-tier member must be REFUSED and NO page created in the target space. ──
	before := pagesIn(targetSpace.ID)
	if c := code(tierReq(http.MethodPost, useURL, "viewer@corp.com", `{"space_id":"`+targetSpace.ID+`"}`)); c != http.StatusForbidden {
		t.Errorf("viewer template-use = %d, want 403 (view-tier cannot create a page in the space)", c)
	}
	if after := pagesIn(targetSpace.ID); after != before {
		t.Errorf("viewer template-use CREATED a page (%d→%d) despite the view tier", before, after)
	}

	// ── (b) POSITIVE: an edit-tier member succeeds and a page IS created. ──
	if c := code(tierReq(http.MethodPost, useURL, "editor@corp.com", `{"space_id":"`+targetSpace.ID+`"}`)); !wrote(c) {
		t.Errorf("editor template-use = %d, want 200/201 (edit grant)", c)
	}
	if after := pagesIn(targetSpace.ID); after != before+1 {
		t.Errorf("editor template-use did not create exactly one page (%d→%d)", before, after)
	}

	// ── (c) Cross-workspace: a space_id in another workspace → 404 (no oracle), and no page created. ──
	if c := code(tierReq(http.MethodPost, useURL, "viewer@corp.com", `{"space_id":"`+foreignSpace.ID+`"}`)); c != http.StatusNotFound {
		t.Errorf("template-use into a foreign-workspace space = %d, want 404 (no oracle)", c)
	}
	if n := pagesIn(foreignSpace.ID); n != 0 {
		t.Errorf("template-use created a page in a foreign-workspace space (%d)", n)
	}
}
