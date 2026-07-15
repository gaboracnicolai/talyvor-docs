package changelog_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// changelog.Create had an INVERTED fallback — the worst shape in the class:
//
//	if in.CreatedBy == "" { in.CreatedBy = authz.ActorOrEmpty(r.Context()) }
//
// It PREFERS the client's value and only consults the verified identity when the client
// omits one. Unlike memberFromReq (which at least preferred the verified actor and needed
// a multi-workspace caller to become exploitable), this is unconditional attribution
// forgery for ANY caller, with no precondition at all.
//
// The workspace override alongside it was fail-open in the other direction:
// `if ws := authz.WorkspaceOrEmpty(ctx); ws != "" { in.WorkspaceID = ws }` silently
// no-ops for a multi-workspace caller (SingleWorkspace → "" for != 1), leaving the body's
// workspace_id in place.

const clSecret = "sec4-test-gateway-secret-0123456789"

func clChain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := changelog.NewHandler(changelog.NewStore(d.Pool, nil)).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(clSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func TestSec_ChangelogCreate_AuthorIsVerifiedNotBody(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	wsVictim := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	mallory := d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Release notes")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	// Mallory needs Edit to reach the route.
	if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: spaceID,
		SubjectType: "member", SubjectID: mallory,
		Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: alice,
	}); err != nil {
		t.Fatal(err)
	}

	chain := clChain(d)
	// Mallory claims Alice authored the entry, and names the victim's workspace.
	body := `{"version":"1.2.3","title":"forged","entry_type":"added","created_by":"` + alice + `","workspace_id":"` + wsVictim + `"}`
	req := httptest.NewRequest(http.MethodPost,
		"/v1/spaces/"+spaceID+"/pages/"+pageID+"/changelog/entries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", clSecret)
	req.Header.Set("X-User-Email", "mallory@corp.com")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create entry = %d, want 2xx (must still work for a legitimate caller). body=%s", rr.Code, rr.Body.String())
	}

	var gotBy, gotWS string
	if err := d.Pool.QueryRow(ctx,
		`SELECT created_by, workspace_id FROM changelog_entries WHERE page_id=$1 AND title='forged'`,
		pageID).Scan(&gotBy, &gotWS); err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if gotBy == alice {
		t.Errorf("ATTRIBUTION FORGERY: created_by = %q (Alice) — the INVERTED fallback preferred "+
			"the client's value outright, with no precondition", gotBy)
	}
	if gotBy != mallory {
		t.Errorf("created_by = %q, want the verified caller %q", gotBy, mallory)
	}
	if gotWS == wsVictim {
		t.Errorf("TENANCY FORGERY: workspace_id = the body's claim %q (the victim's)", gotWS)
	}
	if gotWS != ws {
		t.Errorf("workspace_id = %q, want the parent page's workspace %q", gotWS, ws)
	}
}
