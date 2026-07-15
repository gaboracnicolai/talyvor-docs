package pagelink_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelink"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// pagelink.Create decoded PageLink straight from the body and passed it to Upsert with
// only PageID overridden — so workspace_id AND created_by were client-supplied, and the
// handler imported no authz at all.
//
// workspace_id on page_links is the tenancy key (the table has no FK — it is an opaque
// Track id), so a caller could write a link row tagged with ANOTHER tenant's workspace.
//
// Both now derive from the resource the route already authorized: the workspace is the
// parent PAGE's (permission.WorkspaceFromContext) and created_by is the caller's member
// id in it (permission.ActorFromContext).

const linkSecret = "sec4-test-gateway-secret-0123456789"

func linkChain(d *testutil.DB) http.Handler {
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
	h := pagelink.NewHandler(pagelink.NewStore(d.Pool)).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(linkSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func TestSec_PageLinkCreate_TenancyAndActorAreVerified(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	wsVictim := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	victim := d.Member(t, wsVictim, "victim@corp.com")
	pageID := d.Page(t, ws, alice, "Spec")

	chain := linkChain(d)
	body := `{"issue_id":"ENG-1","link_type":"spec","workspace_id":"` + wsVictim + `","created_by":"` + victim + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/pages/"+pageID+"/links", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", linkSecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create link = %d, want 2xx (must still work for a legitimate caller). body=%s", rr.Code, rr.Body.String())
	}

	var gotWS, gotBy string
	if err := d.Pool.QueryRow(ctx,
		`SELECT workspace_id, created_by FROM page_links WHERE page_id=$1 AND issue_id='ENG-1'`,
		pageID).Scan(&gotWS, &gotBy); err != nil {
		t.Fatalf("read link: %v", err)
	}

	if gotWS == wsVictim {
		t.Errorf("TENANCY FORGERY: link row written with the body's workspace_id %q (the victim's) — "+
			"workspace_id is the tenancy key on page_links and must be derived from the parent page", gotWS)
	}
	if gotWS != ws {
		t.Errorf("workspace_id = %q, want the parent page's workspace %q", gotWS, ws)
	}
	if gotBy == victim {
		t.Errorf("ATTRIBUTION FORGERY: created_by = the body's claim %q", gotBy)
	}
	if gotBy != alice {
		t.Errorf("created_by = %q, want the verified caller %q", gotBy, alice)
	}
}
