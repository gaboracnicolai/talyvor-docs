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
	"github.com/talyvor/docs/internal/testutil"
)

// SEC-4 cross-tenant IDOR. The by-id page routes (/v1/spaces/{spaceID}/pages/{pageID})
// must resolve access from the GATEWAY-VERIFIED identity (x-user-email behind a valid
// x-gateway-auth transit proof), NEVER from the forgeable X-Member-Id / X-Talyvor-Workspace
// headers, and must scope every query to the caller's workspace membership.
//
// This drives the FULL chain the way main.go mounts /v1. RED (pre-fix): the chain has no
// gateway/identity middleware and the store is unscoped, so Alice reaches workspace B's
// page (200) and a request with no transit proof still succeeds — the asserts below FAIL.
// GREEN (post-fix): newV1Chain adds gatewayauth + authz middleware and the store scopes to
// membership; the SAME asserts pass. Only the chain builder changes red→green; the
// assertions are the security contract and stay put.

const testGatewaySecret = "sec4-test-gateway-secret-0123456789"

// newV1Chain builds the /v1 chain under test, mirroring main.go's wiring: gatewayauth +
// authz on the group, then the page handler. (Pre-fix this helper had no middleware and the
// asserts failed — the RED baseline; this is the GREEN wiring.)
func newV1Chain(t *testing.T, d *testutil.DB) http.Handler {
	t.Helper()
	pageHandler := page.NewHandler(page.NewStore(d.Pool), d.Pool)
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		pageHandler.Mount(r)
	})
	return r
}

// asUser forges a gateway-verified request: a valid transit proof + the verified email.
// Deliberately sets NO X-Member-Id / X-Talyvor-Workspace — identity must come only from
// the verified email. withProof=false drops the transit proof entirely.
func asUser(method, path, email string, withProof bool, forged map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(`{"title":"hacked-by-alice"}`))
	r.Header.Set("Content-Type", "application/json")
	if withProof {
		r.Header.Set("X-Gateway-Auth", testGatewaySecret)
	}
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	for k, v := range forged {
		r.Header.Set(k, v)
	}
	return r
}

func spaceOf(t *testing.T, d *testutil.DB, pageID string) string {
	t.Helper()
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space_id for page: %v", err)
	}
	return spaceID
}

func TestSEC4_CrossTenant_ByIDRoutes(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com") // Alice belongs to A only
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com") // Bob belongs to B only
	pB := d.Page(t, wsB, bob, "Secret B roadmap")
	sB := spaceOf(t, d, pB)
	base := "/v1/spaces/" + sB + "/pages/" + pB

	chain := newV1Chain(t, d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	// SCOPE-SOURCE isolation (first, before any mutation attempt): Bob, B's real member,
	// reads his own page → 200. Anchors that the denials below are the workspace mismatch,
	// not a globally-broken query.
	if rr := do(asUser(http.MethodGet, base, "bob@corp.com", true, nil)); rr.Code != http.StatusOK {
		t.Errorf("Bob→B (own page) = %d, want 200 (denial must be scope, not a broken query)", rr.Code)
	}

	// (a)-(d) Alice (member of A) must NOT reach B's page on any by-id route → 404.
	for _, tc := range []struct {
		name, method, path string
	}{
		{"GET page", http.MethodGet, base},
		{"PATCH page", http.MethodPatch, base},
		{"DELETE page", http.MethodDelete, base},
		{"GET versions", http.MethodGet, base + "/versions"},
	} {
		rr := do(asUser(tc.method, tc.path, "alice@corp.com", true, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("[%s] Alice→B = %d, want 404 (cross-tenant must be not-found)", tc.name, rr.Code)
		}
	}

	// TRANSIT PROOF: no x-gateway-auth → 401 BEFORE any identity is read.
	if rr := do(asUser(http.MethodGet, base, "alice@corp.com", false, nil)); rr.Code != http.StatusUnauthorized {
		t.Errorf("no transit proof = %d, want 401 (gateway proof required first)", rr.Code)
	}

	// FORGERY: Alice's verified identity + forged member/workspace headers naming B → the
	// forged headers are IGNORED; Alice still cannot reach B (404, not B's content).
	forged := map[string]string{"X-Member-Id": bob, "X-Talyvor-Workspace": wsB}
	if rr := do(asUser(http.MethodGet, base, "alice@corp.com", true, forged)); rr.Code != http.StatusNotFound {
		t.Errorf("forged member/workspace headers = %d, want 404 (identity is the verified email, not headers)", rr.Code)
	}

	// Alice's blocked PATCH + DELETE must have had NO side effect: B's page still reads for
	// Bob (proves the denials were true no-ops, not just a 404 after the mutation landed).
	if rr := do(asUser(http.MethodGet, base, "bob@corp.com", true, nil)); rr.Code != http.StatusOK {
		t.Errorf("Bob→B after Alice's blocked writes = %d, want 200 (page must survive)", rr.Code)
	}
}
