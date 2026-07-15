package page_test

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
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// SEC-4 cross-tenant WRITE via a body-supplied workspace_id — the twin of the
// POST /v1/spaces hole closed in Run 1.
//
// page.Handler.Create decodes model.Page from the body and overrides ONLY SpaceID.
// model.Page carries workspace_id and created_by, and page.Store.Create REQUIRES a
// non-empty WorkspaceID (rather than deriving it from the space) and inserts it verbatim.
//
// So POST /v1/spaces/{a-space-I-can-edit}/pages with {"workspace_id":"<victim>"} lands a
// row whose workspace_id is the victim's. Every SEC-4 L2 query filters
// `workspace_id = ANY(verified set)`, so the page surfaces in the VICTIM's search and
// stale reports — attacker-authored content, falsely attributed, inside another tenant,
// and invisible to the attacker themselves.
//
// The fix mirrors space/handler.go's Create: derive the workspace from the parent space
// (which the route already authorized via spaceEnf) and take created_by from the verified
// identity. Body values ignored.

// createChain mirrors main.go's wiring for the space-scoped page routes: gatewayauth +
// authz, with Create/List gated by spaceEnf on {spaceID}.
func createChain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
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
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := page.NewHandler(pageStore, d.Pool)
	h.WithAccess(pageEnf, spaceEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func createReq(path, email, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", testGatewaySecret)
	r.Header.Set("X-User-Email", email)
	return r
}

func TestSec_PageCreate_BodyWorkspaceIDCannotCrossTenant(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsAttacker := d.Workspace(t)
	wsVictim := d.Workspace(t)

	mallory := d.Member(t, wsAttacker, "mallory@corp.com") // attacker: her own workspace only
	victim := d.Member(t, wsVictim, "victim@corp.com")
	d.Page(t, wsVictim, victim, "Victim's real page")

	// Mallory's own space, which she created → she legitimately admins it.
	attackerPage := d.Page(t, wsAttacker, mallory, "Mallory's page")
	var attackerSpace string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, attackerPage).Scan(&attackerSpace); err != nil {
		t.Fatal(err)
	}

	chain := createChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	countIn := func(ws, title string) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pages WHERE workspace_id=$1 AND title=$2`, ws, title).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// THE PLANT: create in MY space, but name the VICTIM's workspace in the body.
	body := `{"workspace_id":"` + wsVictim + `","title":"planted-by-mallory","created_by":"` + victim + `"}`
	rr := do(createReq("/v1/spaces/"+attackerSpace+"/pages", "mallory@corp.com", body))

	if n := countIn(wsVictim, "planted-by-mallory"); n != 0 {
		t.Errorf("CROSS-TENANT WRITE: Mallory planted %d page(s) into the victim's workspace %q "+
			"(HTTP %d). The body's workspace_id was inserted verbatim; SEC-4 L2 filters on "+
			"workspace_id, so this page now surfaces in the VICTIM's search/stale reports.",
			n, wsVictim, rr.Code)
	}

	// If the create is allowed at all, it must land in the CALLER's own workspace —
	// derived from the parent space, never from the body.
	if rr.Code == http.StatusCreated || rr.Code == http.StatusOK {
		if n := countIn(wsAttacker, "planted-by-mallory"); n != 1 {
			t.Errorf("create returned %d but the row is not in the caller's own workspace %q "+
				"(found %d) — the workspace must be derived from the parent space", rr.Code, wsAttacker, n)
		}
		var createdBy string
		if err := d.Pool.QueryRow(ctx,
			`SELECT created_by FROM pages WHERE workspace_id=$1 AND title='planted-by-mallory'`,
			wsAttacker).Scan(&createdBy); err == nil {
			if createdBy == victim {
				t.Errorf("created_by = the body's claim (%q, the victim's member id) — attribution "+
					"must come from the verified identity; resolveAccess treats a creator as admin", createdBy)
			}
			if createdBy != mallory {
				t.Errorf("created_by = %q, want Mallory's verified member id %q", createdBy, mallory)
			}
		}
	}

	// SCOPE, NOT BREAKAGE: an ordinary create in her own space — with NO workspace_id in
	// the body at all — must still work. The store used to REQUIRE the body to supply it;
	// deriving it is what makes ignoring the body possible.
	if rr := do(createReq("/v1/spaces/"+attackerSpace+"/pages", "mallory@corp.com", `{"title":"mallory-legit"}`)); rr.Code != http.StatusCreated {
		t.Errorf("Mallory's OWN legitimate create (no workspace_id in body) = %d, want 201 — "+
			"the workspace must be DERIVED from the space, not required from the client. body=%s",
			rr.Code, rr.Body.String())
	}
	if n := countIn(wsAttacker, "mallory-legit"); n != 1 {
		t.Errorf("legitimate page landed %d times in the caller's workspace, want 1", n)
	}
}
