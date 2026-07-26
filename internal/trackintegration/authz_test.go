package trackintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
)

// spyTrack is a stub Track that RECORDS every path it was asked for and serves a secret
// title for the victim workspace. Recording the path is the point: the defect is not just
// "Docs returned data", it is that Docs ASKED Track for a workspace the caller never proved
// membership of, under Docs's own service credential. A handler that denies before the
// upstream call leaves this recorder empty.
func spyTrack(t *testing.T, secret string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		issue := map[string]any{
			"id": "i-1", "identifier": "VICTIM-1", "title": secret,
			"status": "in_progress", "priority": 1, "ai_cost_usd": 0,
		}
		// Client.SearchIssues decodes an ARRAY; GetIssue decodes a single object. Serve the
		// shape each caller expects, or a decode failure would look like a denial and the
		// positive control would pass for the wrong reason.
		if strings.HasSuffix(r.URL.Path, "/issues/search") {
			_ = json.NewEncoder(w).Encode([]map[string]any{issue})
			return
		}
		_ = json.NewEncoder(w).Encode(issue)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// call drives one handler invocation with a verified membership in memberWS while naming
// urlWS in the path — the shape authz.Middleware produces for a real request.
func call(h http.HandlerFunc, email, memberWS, urlWS, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(authz.WithMemberships(req.Context(), email,
		[]authz.Membership{{WorkspaceID: memberWS, MemberID: "m-" + memberWS}}))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("wsID", urlWS)
	rctx.URLParams.Add("issueID", "i-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

// GetIssue and Search read {wsID} from the URL and never intersected it with the caller's
// verified memberships, then handed it to Client.GetIssue/SearchIssues — which builds
// /v1/workspaces/{wsID}/... and sends it with Docs's OWN service API key (client.go fetch).
// That is a confused deputy: Track sees a trusted service credential and a workspace the
// caller chose. The other three wsID-reading packages (ai, analytics, freshness) all call
// authz.AuthorizeWorkspace; these two did not.
//
// RED (pre-fix): the victim's issue title comes back AND the spy recorded a request for
// ws-victim. GREEN: 403, nothing leaked, and Track was never asked.
func TestGetIssue_CrossTenant_Denied(t *testing.T) {
	const secret = "SECRET-VICTIM-ISSUE-TITLE"
	srv, seen := spyTrack(t, secret)
	h := NewHandler(New(srv.URL, "docs-service-key"))

	att := call(h.GetIssue, "attacker@a.com", "ws-attacker", "ws-victim",
		"/workspaces/ws-victim/track/issues/i-1")

	if strings.Contains(att.Body.String(), secret) {
		t.Errorf("cross-tenant LEAK: member of ws-attacker read ws-victim's issue (status %d): %s",
			att.Code, att.Body.String())
	}
	// The load-bearing assertion: Docs must not spend its service credential on an
	// unauthorized workspace, whatever it does with the response.
	for _, p := range seen() {
		if strings.Contains(p, "ws-victim") {
			t.Errorf("confused deputy: Docs asked Track for %q using its own service key, "+
				"for a workspace the caller never proved membership of", p)
		}
	}
	if att.Code != http.StatusForbidden && att.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GetIssue = %d, want 403/404", att.Code)
	}
}

func TestSearchIssues_CrossTenant_Denied(t *testing.T) {
	const secret = "SECRET-VICTIM-ISSUE-TITLE"
	srv, seen := spyTrack(t, secret)
	h := NewHandler(New(srv.URL, "docs-service-key"))

	att := call(h.Search, "attacker@a.com", "ws-attacker", "ws-victim",
		"/workspaces/ws-victim/track/search?q=secret")

	if strings.Contains(att.Body.String(), secret) {
		t.Errorf("cross-tenant LEAK via search (status %d): %s", att.Code, att.Body.String())
	}
	for _, p := range seen() {
		if strings.Contains(p, "ws-victim") {
			t.Errorf("confused deputy: Docs searched Track for %q under its own service key", p)
		}
	}
	if att.Code != http.StatusForbidden && att.Code != http.StatusNotFound {
		t.Errorf("cross-tenant Search = %d, want 403/404", att.Code)
	}
}

// POSITIVE CONTROL. Denial is only correct if the legitimate path still works — otherwise
// "deny everything" would pass the two tests above. A verified member of the workspace they
// name must still reach Track and get the data.
func TestTrackIntegration_MemberStillReads(t *testing.T) {
	const secret = "SECRET-VICTIM-ISSUE-TITLE"
	srv, seen := spyTrack(t, secret)
	h := NewHandler(New(srv.URL, "docs-service-key"))

	ok := call(h.GetIssue, "owner@victim.com", "ws-victim", "ws-victim",
		"/workspaces/ws-victim/track/issues/i-1")
	if !strings.Contains(ok.Body.String(), secret) {
		t.Errorf("a member of ws-victim must read their own issue, got %d: %s", ok.Code, ok.Body.String())
	}
	var asked bool
	for _, p := range seen() {
		if strings.Contains(p, "ws-victim") {
			asked = true
		}
	}
	if !asked {
		t.Error("member request should have reached Track; the spy recorded no ws-victim path")
	}

	okS := call(h.Search, "owner@victim.com", "ws-victim", "ws-victim",
		"/workspaces/ws-victim/track/search?q=secret")
	if !strings.Contains(okS.Body.String(), secret) {
		t.Errorf("a member of ws-victim must search their own issues, got %d: %s", okS.Code, okS.Body.String())
	}
}

// An unconfigured Track must stay a quiet no-op — Docs runs standalone, and this is the
// property that made the live defect inert on a deploy without DOCS_TRACK_API_KEY. The
// authorization added below must not turn that into an error for a legitimate member.
func TestTrackIntegration_UnconfiguredStillNoOps(t *testing.T) {
	h := NewHandler(New("", ""))
	rr := call(h.GetIssue, "owner@victim.com", "ws-victim", "ws-victim",
		"/workspaces/ws-victim/track/issues/i-1")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"configured":false`) {
		t.Errorf("unconfigured GetIssue = %d %s, want 200 {\"configured\":false}", rr.Code, rr.Body.String())
	}
}
