package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// SEC-4 cross-tenant IDOR on the two WORKSPACE-scoped page routes.
//
// Every by-id page route resolves scope from authz.WorkspaceIDs(ctx) (the verified
// membership set) — see TestSEC4_CrossTenant_ByIDRoutes. These two do not: they read
// {wsID} straight out of the URL, an attacker-controlled path param, and hand it to
// the store with no membership check:
//
//	GET /v1/workspaces/{wsID}/pages/search   → store.Search(ctx, wsID, q, limit)
//	GET /v1/workspaces/{wsID}/pages/stale    → store.GetStalePages(ctx, wsID)
//
// They were missed because the SEC-4 sweep enumerated packages by hand (6050788 did
// "the 7 secondary handler groups"; 06adb69 did "search/ai/freshness/analytics/
// importer") and the page package's workspace routes were in neither list. The sibling
// route /v1/workspaces/{wsID}/search (internal/search/handler.go) guards this exact
// shape with authz.AuthorizeWorkspace and says why: "a member of any workspace could
// read another workspace's document body text". These two must do the same.
//
// RED (pre-fix): Alice, a member of A only, reads B's page bodies — 200 + B's title.
// GREEN (post-fix): 403 on both, while Bob's OWN workspace still returns his data —
// so the denial is scope, not a broken query.
func TestSEC4_WorkspaceRoutes_SearchAndStale_CrossTenant(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com") // Alice belongs to A only
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com") // Bob belongs to B only

	const secretTitle = "Secret B acquisition roadmap"
	pB := d.Page(t, wsB, bob, secretTitle)

	// Make B's page genuinely stale, so GetStalePages returns a ROW. Without this the
	// stale route returns [] for everyone and the test could not tell a leak from a
	// denial — the assertion would pass for the wrong reason.
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE pages SET stale_after_days = 1, updated_at = NOW() - INTERVAL '30 days'
		 WHERE id = $1`, pB); err != nil {
		t.Fatalf("seed stale page: %v", err)
	}

	chain := newV1Chain(t, d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	searchB := "/v1/workspaces/" + wsB + "/pages/search?q=acquisition"
	staleB := "/v1/workspaces/" + wsB + "/pages/stale"
	searchA := "/v1/workspaces/" + wsA + "/pages/search?q=acquisition"

	// ANCHOR (first, before any denial assert): Bob, B's real member, reads his own
	// workspace and actually gets the page back. This proves the denials below are the
	// workspace mismatch — not a globally-broken query or an empty fixture.
	if rr := do(asUser(http.MethodGet, searchB, "bob@corp.com", true, nil)); rr.Code != http.StatusOK {
		t.Errorf("Bob→B search (own workspace) = %d, want 200 (denial must be scope, not a broken query)", rr.Code)
	} else if !strings.Contains(rr.Body.String(), secretTitle) {
		t.Errorf("Bob→B search returned 200 but not his own page — fixture is wrong, the leak asserts below would pass for the wrong reason. body=%s", rr.Body.String())
	}
	if rr := do(asUser(http.MethodGet, staleB, "bob@corp.com", true, nil)); rr.Code != http.StatusOK {
		t.Errorf("Bob→B stale (own workspace) = %d, want 200", rr.Code)
	} else if !strings.Contains(rr.Body.String(), secretTitle) {
		t.Errorf("Bob→B stale returned 200 but not his own stale page — fixture is wrong. body=%s", rr.Body.String())
	}

	// (a) Alice must NOT full-text search B's page bodies.
	if rr := do(asUser(http.MethodGet, searchB, "alice@corp.com", true, nil)); rr.Code != http.StatusForbidden {
		t.Errorf("Alice→B pages/search = %d, want 403 (cross-tenant read of another workspace's page bodies). body=%s",
			rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), secretTitle) {
		t.Errorf("Alice→B pages/search LEAKED B's page title in a %d response: %s", rr.Code, rr.Body.String())
	}

	// (b) Alice must NOT read B's stale-page report.
	if rr := do(asUser(http.MethodGet, staleB, "alice@corp.com", true, nil)); rr.Code != http.StatusForbidden {
		t.Errorf("Alice→B pages/stale = %d, want 403 (cross-tenant read of another workspace's pages). body=%s",
			rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), secretTitle) {
		t.Errorf("Alice→B pages/stale LEAKED B's page title in a %d response: %s", rr.Code, rr.Body.String())
	}

	// (c) SCOPE, NOT BREAKAGE: Alice's OWN workspace still works. A guard that 403s
	// everything would pass (a)+(b) while breaking the product — this catches that.
	if rr := do(asUser(http.MethodGet, searchA, "alice@corp.com", true, nil)); rr.Code != http.StatusOK {
		t.Errorf("Alice→A search (own workspace) = %d, want 200 (the guard must scope, not blanket-deny)", rr.Code)
	}

	// (d) TRANSIT PROOF: no x-gateway-auth → 401 before any identity is read.
	if rr := do(asUser(http.MethodGet, searchB, "alice@corp.com", false, nil)); rr.Code != http.StatusUnauthorized {
		t.Errorf("no transit proof = %d, want 401", rr.Code)
	}

	// (e) FORGERY: a forged workspace header naming B must not buy Alice access —
	// membership comes from the verified email, never a client-supplied header.
	forged := map[string]string{"X-Talyvor-Workspace": wsB, "X-Member-Id": bob}
	if rr := do(asUser(http.MethodGet, searchB, "alice@corp.com", true, forged)); rr.Code != http.StatusForbidden {
		t.Errorf("forged workspace/member headers = %d, want 403 (identity is the verified email)", rr.Code)
	}
}

// SEC-4: a caller with a valid transit proof but ZERO memberships (authz.Middleware
// proceeds with an empty set rather than 401ing) must not be able to read any
// workspace. Empty-set callers are the weakest possible identity and are the reason
// AuthorizeWorkspace must be called explicitly — WorkspaceIDs(ctx) returning [] makes
// `= ANY($n)` match nothing on the by-id routes, but these routes never consult it.
func TestSEC4_WorkspaceRoutes_ZeroMembershipCallerDenied(t *testing.T) {
	d := testutil.New(t)

	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	d.Page(t, wsB, bob, "Secret B acquisition roadmap")

	chain := newV1Chain(t, d)
	rr := httptest.NewRecorder()
	// nobody@ is gateway-verified but seeded into NO workspace_members row.
	chain.ServeHTTP(rr, asUser(http.MethodGet,
		"/v1/workspaces/"+wsB+"/pages/search?q=acquisition", "nobody@corp.com", true, nil))

	if rr.Code != http.StatusForbidden {
		t.Errorf("zero-membership caller→B search = %d, want 403. body=%s", rr.Code, rr.Body.String())
	}
	var got []map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &got) == nil && len(got) > 0 {
		t.Errorf("zero-membership caller received %d page(s) from B: %s", len(got), rr.Body.String())
	}
}
