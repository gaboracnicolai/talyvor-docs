package collab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/collab"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// collabPageLooker builds the scoped page+space meta looker the SessionResolver needs — the same
// looker that backs the REST enforcers in main.go.
func collabPageLooker(d *testutil.DB) func(context.Context, string) (permission.PageMeta, error) {
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	return func(ctx context.Context, id string) (permission.PageMeta, error) {
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
}

const testGatewaySecret = "sec4-test-gateway-secret-0123456789"

// SEC-4 collab door: opening a live-edit WebSocket session on a page must require a valid
// gateway transit proof AND membership in the page's workspace. Identity comes from the
// verified gateway headers, never a ?member_id= query param.
//
// RED (pre-fix): newCollabChain mounts collab on the ROOT router with no middleware and
// ServeWS trusts ?member_id=, so Alice opens a session on workspace B's page and a no-proof
// request still upgrades — the asserts below FAIL. GREEN (post-fix): collab moves inside the
// /v1 boundary and ServeWS scopes the page to the caller's membership; the SAME asserts pass.
func newCollabChain(t *testing.T, d *testutil.DB) http.Handler {
	t.Helper()
	// GREEN wiring, mirroring main.go: collab inside the /v1 boundary (gatewayauth + authz),
	// scoped to the caller's workspace membership. (Pre-fix this mounted on the root router
	// with no middleware/scope — the RED baseline.)
	h := collab.NewHandler(collab.NewOTEngine()).
		WithAccess(collab.NewPermissionSession(permission.NewStore(d.Pool), collabPageLooker(d)))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Get("/collab/{pageID}/ws", h.ServeWS)
	})
	return r
}

func TestSEC4_Collab_CrossTenant(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com") // Alice → A only
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pB := d.Page(t, wsB, bob, "B live doc")

	srv := httptest.NewServer(newCollabChain(t, d))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/collab/" + pB + "/ws?client_id=c1"

	// dial returns the HTTP status of the upgrade attempt (101 = connected).
	dial := func(email string, withProof bool, forgedMember string) int {
		h := http.Header{}
		if withProof {
			h.Set("X-Gateway-Auth", testGatewaySecret)
		}
		if email != "" {
			h.Set("X-User-Email", email)
		}
		u := wsURL
		if forgedMember != "" {
			u += "&member_id=" + forgedMember
		}
		conn, resp, _ := websocket.DefaultDialer.Dial(u, h)
		if conn != nil {
			_ = conn.Close()
		}
		if resp == nil {
			return 0
		}
		return resp.StatusCode
	}

	// Bob (member of B) opens a session on his own page → 101 Switching Protocols.
	if code := dial("bob@corp.com", true, ""); code != http.StatusSwitchingProtocols {
		t.Errorf("Bob→B collab = %d, want 101 (own page connects)", code)
	}

	// Alice (member of A), even with a forged member_id naming Bob, must NOT open a session
	// on B's page → 404 (never a live channel into another tenant).
	if code := dial("alice@corp.com", true, bob); code == http.StatusSwitchingProtocols {
		t.Errorf("Alice→B collab upgraded (101) — cross-tenant live-edit hole; want 404")
	} else if code != http.StatusNotFound {
		t.Errorf("Alice→B collab = %d, want 404", code)
	}

	// No transit proof → 401 before any session opens.
	if code := dial("alice@corp.com", false, ""); code != http.StatusUnauthorized {
		t.Errorf("no-proof collab = %d, want 401 (gateway proof required)", code)
	}
}
