package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
)

// stubSearcher returns a canned result carrying a secret headline (the cross-tenant content that would
// leak). It ignores the workspace filter on purpose — the handler is the only thing that should stop a
// non-member from ever reaching the store for another workspace.
type stubSearcher struct{ secret string }

func (s stubSearcher) SearchWithRank(_ context.Context, _ string, _ string, _ *string, _, _ int) ([]page.SearchResult, error) {
	return []page.SearchResult{{SpaceName: "victim-space", Rank: 1, Headline: s.secret}}, nil
}

// A4D: /workspaces/{wsID}/search took wsID from the URL and never intersected the caller's memberships,
// so a member of workspace A read workspace B's document body text (ts_headline). RED: the secret comes
// back. GREEN: a non-member is denied (403/404) and a real member still gets results.
func TestSearch_CrossTenant_Denied(t *testing.T) {
	const secret = "SECRET-CONTENT-FROM-WS-B"
	h := NewHandler(stubSearcher{secret: secret}, nil)

	call := func(email, memberWS, urlWS string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/workspaces/"+urlWS+"/search?q=secret&type=fulltext", nil)
		req = req.WithContext(authz.WithMemberships(req.Context(), email, []authz.Membership{{WorkspaceID: memberWS, MemberID: "m"}}))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("wsID", urlWS)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rr := httptest.NewRecorder()
		h.Search(rr, req)
		return rr
	}

	// Attacker: verified member of ws-A ONLY, names ws-B in the path.
	att := call("attacker@a.com", "ws-A", "ws-B")
	if strings.Contains(att.Body.String(), secret) {
		t.Errorf("cross-tenant LEAK: member of ws-A read ws-B content (status %d): %s", att.Code, att.Body.String())
	}
	if att.Code != http.StatusForbidden && att.Code != http.StatusNotFound {
		t.Errorf("cross-tenant search = %d, want 403/404", att.Code)
	}

	// POSITIVE: a real member of ws-B still gets results.
	ok := call("owner@b.com", "ws-B", "ws-B")
	if !strings.Contains(ok.Body.String(), secret) {
		t.Errorf("member of ws-B should read ws-B content, got %d: %s", ok.Code, ok.Body.String())
	}
}
