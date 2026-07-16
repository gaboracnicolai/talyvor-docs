package editsession_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/editsession"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const esTestSecret = "editsession-test-gateway-secret-0123456789"

// v1EditSessionChain mounts the edit-session handler under the real /v1 chain (gatewayauth →
// authz → permission enforcer), mirroring main.go and the pagelock SEC harness.
func v1EditSessionChain(d *testutil.DB) http.Handler {
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
			WorkspaceID: pg.WorkspaceID,
			SpaceID:     pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := editsession.NewHandler(editsession.NewStore(d.Pool)).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(esTestSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func esReq(method, path, email string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", esTestSecret)
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

// PHASE 2 — HTTP TENANCY: a member of workspace B cannot acquire, observe, take over,
// heartbeat, or release an edit session on a workspace-A page. Every op resolves to 404 (no
// cross-tenant surface, no existence oracle). The owner's own acquire succeeds — so the denials
// are the workspace boundary, not a globally broken route.
func TestEditSession_HTTP_CrossTenant_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com") // Bob is a member of B only

	pA := d.Page(t, wsA, alice, "A's roadmap")
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(), `SELECT space_id FROM pages WHERE id=$1`, pA).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}
	base := "/v1/spaces/" + spaceID + "/pages/" + pA + "/edit-session"

	chain := v1EditSessionChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	// Anchor: A's own member acquires → 200 (denials below are scope, not a broken route).
	if rr := do(esReq(http.MethodPost, base, "alice@corp.com")); rr.Code != http.StatusOK {
		t.Fatalf("owner acquire = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	// Bob (member of B) on A's page → 404 on every edit-session op.
	for _, tc := range []struct{ name, method, path string }{
		{"acquire", http.MethodPost, base},
		{"get", http.MethodGet, base},
		{"takeover", http.MethodPost, base + "/takeover"},
		{"heartbeat", http.MethodPost, base + "/heartbeat"},
		{"release", http.MethodDelete, base},
	} {
		rr := do(esReq(tc.method, tc.path, "bob@corp.com"))
		if rr.Code != http.StatusNotFound {
			t.Errorf("[%s] cross-tenant = %d, want 404 (no cross-tenant edit-session surface). body=%s",
				tc.name, rr.Code, rr.Body.String())
		}
	}

	// A's session survived Bob's attempts, still held by Alice.
	rr := do(esReq(http.MethodGet, base, "alice@corp.com"))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), alice) {
		t.Fatalf("owner Get after cross-tenant attempts = %d body=%s, want 200 holding alice", rr.Code, rr.Body.String())
	}
}
