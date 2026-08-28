package ai_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/ai"
	"github.com/talyvor/docs/internal/authz"
)

// sec_aiwrite_foreign_test.go — ALL FIVE /v1/workspaces/{wsID}/ai/* routes must refuse a caller who
// is not a member of {wsID}.
//
// ⚠ WHY THIS EXISTS: THE CHECKS WERE ALREADY THERE AND NOTHING DEFENDED ANY OF THEM. Each handler
// calls `authz.AuthorizeWorkspace` and returns 403 on !ok, as its first statement, before the body
// is even decoded. MEASURED, not supposed (~/talyvor-queue/w338-authz-verdict-census-k2w8.py):
// neutering the verdict one site at a time — leaving the call in place and never acting on it,
// `; !ok {` → `; false && !ok {` — left the WHOLE SUITE GREEN against a real Postgres for all five.
//
// That census ran the same mutation over every one of this repository's 21
// `authz.AuthorizeWorkspace` verdict sites. 15 were caught by something. The six that were not are
// these five plus `analytics.SpaceStats`, which gains its own twin in this commit.
//
// ⚠⚠ THESE FIVE ARE THE ROUTES THAT SPEND MONEY. Every one of them reaches Lens on the workspace's
// credential and bills the completion to it. An unacted-on verdict here is not only a read of
// someone else's data — it is a stranger spending their AI budget.
//
// ⚠ AND IT IS NOT COVERED BY THE STATIC GUARD NEXT DOOR, which is the more useful half.
// `internal/mountguard`'s enforcer population asserts these handlers REACH `AuthorizeWorkspace`.
// Under the mutation they still do — the call is right there. Reaching a decision and acting on it
// are different properties, and only a request can tell them apart.
//
// ⚠ THE POSITIVE CONTROL IS NOT OPTIONAL. A 403 earned by a malformed request says nothing about
// authorization, so the same call with the caller's OWN workspace must NOT be 403. Without that
// pair each case would pass just as happily against a handler that refused everyone. The own-
// workspace call here falls through to body decoding and answers 4xx-but-not-403, which is exactly
// the distinction being drawn.

// aiForeignCall drives one handler invocation with a verified membership in memberWS while the URL
// names urlWS — the shape authz.Middleware produces for a real request.
func aiForeignCall(h http.HandlerFunc, route, email, memberWS, urlWS string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/v1/workspaces/"+urlWS+"/ai/"+route, strings.NewReader(`{}`))
	req = req.WithContext(authz.WithMemberships(req.Context(), email,
		[]authz.Membership{{WorkspaceID: memberWS, MemberID: "m-" + memberWS}}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("wsID", urlWS)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestAIRoutes_RefuseAWorkspaceTheCallerIsNotIn(t *testing.T) {
	// No engine and no page reader: every one of these refuses BEFORE it decodes a body, so a
	// foreign call never reaches either. If a future edit moved the check below the decode, the
	// own-workspace control would still pass and the foreign assertion would start failing — which
	// is the direction that should be loud.
	h := ai.NewHandler(nil, nil)

	const victim, member = "ws-victim-ai", "ws-caller-ai"

	for _, c := range []struct {
		name  string
		route string
		fn    http.HandlerFunc
	}{
		{"Write", "write", h.Write},
		{"Transform", "transform", h.Transform},
		{"Translate", "translate", h.Translate},
		{"Ask", "ask", h.Ask},
		{"SuggestTitle", "suggest-title", h.SuggestTitle},
	} {
		t.Run(c.name, func(t *testing.T) {
			if rr := aiForeignCall(c.fn, c.route, "caller@x.test", member, victim); rr.Code != http.StatusForbidden {
				t.Errorf("a caller with membership only in %q reached ai/%s for %q and got %d, want 403.\n"+
					"    AuthorizeWorkspace in the handler is the only gate on this route, and every one "+
					"of these bills a Lens completion to the workspace named in the URL — so an unacted-on "+
					"verdict lets a stranger spend %q's AI budget.", member, c.route, victim, rr.Code, victim)
			}

			// POSITIVE CONTROL. Same handler, same shape, the caller's OWN workspace. If this were
			// also 403 the assertion above would be earned by the request being wrong rather than
			// by the gate.
			if rr := aiForeignCall(c.fn, c.route, "caller@x.test", member, member); rr.Code == http.StatusForbidden {
				t.Fatalf("the caller was refused their OWN workspace %q (403) on ai/%s, so the refusal "+
					"above proves nothing about authorization — this case is not armed.", member, c.route)
			}
		})
	}
}
