package trackintegration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/membership"
	"github.com/talyvor/docs/internal/testutil"
	"github.com/talyvor/docs/internal/trackintegration"
)

// The member-sync service route, exercised OVER HTTP THROUGH THE REAL MIDDLEWARE CHAIN.
//
// ⚠ WHY THIS FILE EXISTS, AND WHAT ITS ABSENCE COST. Every other test of this route calls
// SyncOneWorkspace — the function underneath the handler — so the handler was never invoked
// over HTTP and the middleware above it was never measured. The route was green in CI and
// 403ed in production from the day it shipped.
//
// The gap was not that the tests were weak about what they asserted. They were precise:
// Docs asserted "SyncOneWorkspace reconciles exactly that workspace", and the BFF asserted
// "the nudge POSTs this path with X-Gateway-Auth". Both were true in production. What
// neither asserted is that a request carrying EXACTLY those headers is ACCEPTED — and the
// BFF's stub upstream answered 200 unconditionally, so "the nudge succeeds" was a property
// of the stub, never of Docs.
//
// So this test builds the router the way cmd/docs/main.go builds it — same two middlewares,
// same exemption predicate — and sends what the BFF actually sends: the transit proof, and
// no identity headers, because a service call has no user.

const testGatewaySecret = "svc-route-test-gateway-secret-0123456789"

func serviceRouter(t *testing.T, d *testutil.DB) chi.Router {
	t.Helper()
	syncer := trackintegration.NewSyncer(nil, nil, nil, "").
		WithMemberSync(&stubMembers{}, membership.NewStore(d.Pool))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		// The PRODUCTION predicates, imported rather than restated. A copy here could go
		// stale against main.go and this test would then measure a router that does not
		// exist — which is one level up from the bug it was written for.
		r.Use(gatewayauth.Middleware(testGatewaySecret, gatewayauth.ExemptTransitProof))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), gatewayauth.ExemptMembership))
		syncer.MountService(r)
		// A workspace-scoped user route stands beside it as the contrast: whatever this
		// change does for the service lane, it must not loosen the user lane.
		r.Get("/workspaces/{wsID}/probe", func(w http.ResponseWriter, req *http.Request) {
			if _, ok := authz.Memberships(req.Context()); !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
	})
	return r
}

// TestServiceRoute_AcceptsTheBFFsExactRequest is the production shape, verbatim.
//
// The BFF sends POST, Content-Type + Accept, and X-Gateway-Auth. It sends NO X-User-Email:
// the caller is a service, and the workspace it names is one whose membership has not been
// read yet — that is the entire reason this route exists.
func TestServiceRoute_AcceptsTheBFFsExactRequest(t *testing.T) {
	d := testutil.New(t)
	r := serviceRouter(t, d)
	ws := d.Workspace(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/service/workspaces/"+ws+"/member-sync", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	// deliberately no X-User-Email / X-User-Id / X-Auth-Iss

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden {
		t.Fatalf("403 %s — the service route is behind membership authz, and a service call has "+
			"no identity to satisfy it. This is the production failure.", strings.TrimSpace(rec.Body.String()))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want 200", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// TestServiceRoute_StillRequiresTheTransitProof — the exemption must move the boundary, not
// remove it. Without the secret this route must be refused, or exempting it from membership
// authz would have opened it to the internet.
func TestServiceRoute_StillRequiresTheTransitProof(t *testing.T) {
	d := testutil.New(t)
	r := serviceRouter(t, d)
	ws := d.Workspace(t)

	for _, tc := range []struct{ name, secret string }{
		{"no proof", ""},
		{"wrong proof", "not-the-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/service/workspaces/"+ws+"/member-sync", nil)
			if tc.secret != "" {
				req.Header.Set("X-Gateway-Auth", tc.secret)
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Fatalf("%s reached the handler — the service lane is authenticated by the "+
					"SECRET, and without it there is nothing left guarding this route", tc.name)
			}
		})
	}
}

// TestUserRoute_StillRequiresIdentity is the blast-radius check. The fix exempts a PREFIX,
// so the thing to prove is that the prefix did not widen: an ordinary /v1 route with a valid
// transit proof and no identity must still be refused.
func TestUserRoute_StillRequiresIdentity(t *testing.T) {
	d := testutil.New(t)
	r := serviceRouter(t, d)
	ws := d.Workspace(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/probe", nil)
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("a user route with no identity returned %d, want 403 — the service exemption "+
			"has leaked into the user lane", rec.Code)
	}
}

// stubMembers satisfies the member source without a live Track. memberSyncOn() requires it to
// report configured, or the handler answers 503 and this file would be testing that instead.
type stubMembers struct{}

func (stubMembers) MemberSyncConfigured() bool { return true }
func (stubMembers) GetWorkspaceMembers(_ context.Context, _ string) ([]membership.MemberRef, error) {
	return nil, nil
}
func (stubMembers) ListWorkspaceIDs(_ context.Context) ([]string, error) { return nil, nil }
