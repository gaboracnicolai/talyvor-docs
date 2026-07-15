package analytics_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// analytics.RecordView took viewer_id from the request BODY and never overrode it — while
// overriding workspace_id on the very next line. viewer_id feeds
// COUNT(DISTINCT viewer_id) and GROUP BY viewer_id, so it forges "who read this page".
//
// This file also settles the ROUTE COLLISION: POST /v1/spaces/{s}/pages/{p}/view is
// registered by BOTH page.Mount (nested under r.Route("/spaces/{spaceID}/pages")) and
// analytics.Mount (absolute path), and main.go mounts page first, analytics second. Only
// one can serve. page.RecordView resolves its viewer from the verified identity; the
// analytics one did not — so if analytics shadows page, the SAFE handler is dead code and
// the unsafe one is live. chainBoth below mirrors main.go's exact mount order so the test
// observes whatever production actually does.

const anSecret = "sec4-test-gateway-secret-0123456789"

// chainBoth mirrors main.go: page.Mount THEN analytics.Mount, both behind the same
// gatewayauth + authz + pageEnf wiring.
func chainBoth(d *testutil.DB) http.Handler {
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
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID",
		func(ctx context.Context, id string) (permission.SpaceMeta, error) {
			sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
			if err != nil {
				return permission.SpaceMeta{}, err
			}
			return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
		}))

	ph := page.NewHandler(pageStore, d.Pool)
	ph.WithAccess(pageEnf, spaceEnf)
	ah := analytics.NewHandler(analytics.NewStore(d.Pool))
	ah.WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(anSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		ph.Mount(r) // main.go order: page first…
		ah.Mount(r) // …analytics second
	})
	return r
}

func TestSec_RecordView_ViewerIsVerifiedNotBody(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	mallory := d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Spec")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}

	chain := chainBoth(d)
	// duration_sec must clear analytics' 3s minimum, or the row is silently dropped and
	// the test could not tell "filtered" from "not recorded by this handler".
	body := `{"viewer_id":"` + alice + `","viewer_name":"Alice","duration_sec":10}`
	req := httptest.NewRequest(http.MethodPost, "/v1/spaces/"+spaceID+"/pages/"+pageID+"/view", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", anSecret)
	req.Header.Set("X-User-Email", "mallory@corp.com")
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("record view = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	// Which handler served it? analytics.RecordView inserts a page_views row; the page
	// package's RecordView does not.
	var views int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM page_views WHERE page_id=$1`, pageID).Scan(&views); err != nil {
		t.Fatal(err)
	}
	t.Logf("ROUTE COLLISION: page_views rows after POST .../view = %d "+
		"(>0 ⇒ analytics.RecordView is live and page.RecordView is shadowed dead code)", views)

	if views == 0 {
		// page.RecordView won: it resolves the viewer from the verified identity, so
		// there is no forgery surface — but then analytics records nothing at all.
		t.Skip("analytics.RecordView is shadowed by page.RecordView — no page_views row written; " +
			"see the reconciliation note in BUILD_STATE")
	}

	var viewer string
	if err := d.Pool.QueryRow(ctx,
		`SELECT viewer_id FROM page_views WHERE page_id=$1`, pageID).Scan(&viewer); err != nil {
		t.Fatal(err)
	}
	if viewer == alice {
		t.Errorf("READERSHIP FORGERY: page_views.viewer_id = %q (Alice) — Mallory recorded a view "+
			"AS Alice from the request body. viewer_id feeds COUNT(DISTINCT viewer_id), so this "+
			"forges who read the page.", viewer)
	}
	if viewer != mallory {
		t.Errorf("viewer_id = %q, want the verified caller %q", viewer, mallory)
	}
}
