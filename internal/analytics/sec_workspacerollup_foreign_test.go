package analytics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/testutil"
)

// sec_workspacerollup_foreign_test.go — GET /v1/workspaces/{wsID}/analytics/pages must refuse a
// caller who is not a member of {wsID}.
//
// ⚠ WHY THIS EXISTS: THE CHECK WAS ALREADY THERE AND NOTHING DEFENDED IT. `WorkspaceStats` calls
// `authz.AuthorizeWorkspace` and returns 403 on !ok, and that route carries NO permission enforcer
// — the workspace is the only object in the path, so there is nothing for a resolver to gate on.
// The handler's own `if` is therefore the ONE thing between a member of workspace B and workspace
// A's page-view roll-up.
//
// MEASURED, not supposed: neutering the verdict — leaving the call in place and never acting on it,
// `; !ok {` → `; false && !ok {` — left ALL 45 PACKAGES GREEN against a real Postgres. A one-token
// edit, and the whole repository agreed. That is what this test is for, and the same measurement is
// why `internal/freshness` gained its twin in the same commit.
//
// ⚠⚠ AND IT IS NOT COVERED BY THE STATIC GUARD NEXT DOOR, which is the more useful half.
// `internal/mountguard/enforcer_population_test.go` asserts this handler REACHES
// `AuthorizeWorkspace`. Under the mutation above it still does — the call is right there. Reaching
// a decision and acting on it are different properties and only a request can tell them apart.
//
// ⚠ THE POSITIVE CONTROL IS NOT OPTIONAL. A 403 earned by a malformed request says nothing about
// authorization, so the same call with the caller's OWN workspace must NOT be 403. Without that
// pair this test would pass just as happily against a handler that refused everyone.

// foreignCall drives one handler invocation with a verified membership in memberWS while the URL
// names urlWS — the shape authz.Middleware produces for a real request.
func foreignCall(h http.HandlerFunc, email, memberWS, urlWS string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+urlWS+"/analytics/pages", nil)
	req = req.WithContext(authz.WithMemberships(req.Context(), email,
		[]authz.Membership{{WorkspaceID: memberWS, MemberID: "m-" + memberWS}}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("wsID", urlWS)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestWorkspaceStats_RefusesAWorkspaceTheCallerIsNotIn(t *testing.T) {
	d := testutil.New(t)
	h := NewHandler(NewStore(d.Pool))

	const victim, member = "ws-victim-analytics", "ws-caller-analytics"

	if rr := foreignCall(h.WorkspaceStats, "caller@x.test", member, victim); rr.Code != http.StatusForbidden {
		t.Errorf("a caller with membership only in %q read the roll-up for %q and got %d, want 403.\n"+
			"    This route carries no permission enforcer — AuthorizeWorkspace in the handler is "+
			"the only gate on it, and a workspace-wide page-view roll-up is exactly the kind of "+
			"thing a non-member must not see.", member, victim, rr.Code)
	}

	// POSITIVE CONTROL. Same handler, same shape, the caller's OWN workspace. If this were also
	// 403 the assertion above would be earned by the request being wrong rather than by the gate.
	if rr := foreignCall(h.WorkspaceStats, "caller@x.test", member, member); rr.Code == http.StatusForbidden {
		t.Fatalf("the caller was refused their OWN workspace %q (403), so the refusal above proves "+
			"nothing about authorization — this test is not armed.", member)
	}
}
