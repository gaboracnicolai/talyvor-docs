package freshness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
)

// sec_workspacefreshness_foreign_test.go — GET /v1/workspaces/{wsID}/freshness must refuse a caller
// who is not a member of {wsID}.
//
// ⚠ THE TWIN OF internal/analytics/sec_workspacerollup_foreign_test.go, and it exists for the same
// measured reason. `Workspace` calls `authz.AuthorizeWorkspace` and 403s on !ok; the route carries
// no permission enforcer, because the workspace is the only object in the path. So this one `if` is
// everything standing between a member of workspace B and workspace A's stale-page report — a list
// of page titles and how long each has gone unverified.
//
// MEASURED: neutering the verdict while leaving the call in place (`; !ok {` → `; false && !ok {`)
// left ALL 45 PACKAGES GREEN against a real Postgres. `internal/trackintegration` — the fourth
// package with this same shape — reded immediately, because it has had a cross-workspace test since
// its confused-deputy fix. analytics and freshness did not. Its authz_test.go even names all three
// of us as callers of AuthorizeWorkspace; only the author's own package was defended.
//
// ⚠⚠ THE STATIC GUARD DOES NOT COVER THIS. `mountguard/enforcer_population_test.go` asserts the
// handler REACHES AuthorizeWorkspace, and under the mutation above it still does. Reaching a
// decision and acting on it are different properties, and only a request separates them.

func freshForeignCall(h http.HandlerFunc, email, memberWS, urlWS string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+urlWS+"/freshness", nil)
	req = req.WithContext(authz.WithMemberships(req.Context(), email,
		[]authz.Membership{{WorkspaceID: memberWS, MemberID: "m-" + memberWS}}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("wsID", urlWS)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestWorkspaceFreshness_RefusesAWorkspaceTheCallerIsNotIn(t *testing.T) {
	// The package's own in-memory readers: the refusal must happen BEFORE the stale report is
	// built, so a real Postgres would add nothing here — and the positive control below still
	// reaches the engine, which is the point of it.
	h := NewHandler(newEngine(&fakePageStore{}, &fakeLinks{}, &fakeTrack{}))

	const victim, member = "ws-victim-freshness", "ws-caller-freshness"

	if rr := freshForeignCall(h.Workspace, "caller@x.test", member, victim); rr.Code != http.StatusForbidden {
		t.Errorf("a caller with membership only in %q read the stale report for %q and got %d, "+
			"want 403. The stale report lists page titles and how long each has gone unverified; "+
			"AuthorizeWorkspace in this handler is the only gate on the route.", member, victim, rr.Code)
	}

	// POSITIVE CONTROL — see the analytics twin. A refusal that also fires on the caller's own
	// workspace is a broken request, not a working gate.
	if rr := freshForeignCall(h.Workspace, "caller@x.test", member, member); rr.Code == http.StatusForbidden {
		t.Fatalf("the caller was refused their OWN workspace %q (403); this test is not armed.", member)
	}
}
