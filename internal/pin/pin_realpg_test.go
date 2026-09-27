package pin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/pin"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const pinTestSecret = "pin-test-gateway-secret-long-enough"

// chain is the shipped /v1 chain for this package: gateway proof → verified memberships → the page
// enforcer on pin/unpin → the handler, with the same per-page View check main.go wires for the lists.
// Each call builds a new one, so nothing a previous "session" did can survive in memory.
func chain(d *testutil.DB) http.Handler {
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
		return permission.PageMeta{WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := pin.NewHandler(pin.NewStore(d.Pool)).WithAccess(pageEnf, func(ctx context.Context, memberID, pageID string) bool {
		md, err := pageLooker(ctx, pageID)
		if err != nil {
			return false
		}
		lvl, err := permStore.CheckPage(ctx, memberID, pageID, md, authz.WorkspaceIDs(ctx))
		return err == nil && permission.AtLeast(lvl, permission.AccessView)
	})
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(pinTestSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func call(t *testing.T, h http.Handler, method, path, email string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-Gateway-Auth", pinTestSecret)
	r.Header.Set("X-User-Email", email)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func titles(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rr.Code, rr.Body.String())
	}
	var got []pin.Entry
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("list body: %v", err)
	}
	out := []string{}
	for _, e := range got {
		out = append(out, e.Title)
	}
	return out
}

// B18.41 — a pin saved through the API comes back for the same member in a later session (a new
// chain, nothing in memory), and only for them; it is not listed once they can no longer view the
// page; unpinning removes it; and the pages they opened come back as their recent pages.
func TestPins_FollowTheMemberAndOnlyTheMember_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	pageID := d.Page(t, ws, alice, "Runbook")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}
	pinPath := "/v1/spaces/" + spaceID + "/pages/" + pageID + "/pin"
	pins := "/v1/workspaces/" + ws + "/pins"

	if rr := call(t, chain(d), http.MethodPut, pinPath, "bob@corp.com"); rr.Code != http.StatusOK {
		t.Fatalf("Bob pins = %d: %s", rr.Code, rr.Body.String())
	}
	if got := titles(t, call(t, chain(d), http.MethodGet, pins, "bob@corp.com")); len(got) != 1 || got[0] != "Runbook" {
		t.Fatalf("Bob's pins in a new session = %v, want [Runbook]", got)
	}
	if got := titles(t, call(t, chain(d), http.MethodGet, pins, "alice@corp.com")); len(got) != 0 {
		t.Fatalf("Alice's pins = %v, want none — a pin is its member's", got)
	}

	// The space goes private and Bob holds no grant: the pin stays stored but is not listed.
	if _, err := d.Pool.Exec(ctx, `UPDATE spaces SET private = true WHERE id = $1`, spaceID); err != nil {
		t.Fatalf("make private: %v", err)
	}
	if got := titles(t, call(t, chain(d), http.MethodGet, pins, "bob@corp.com")); len(got) != 0 {
		t.Fatalf("Bob's pins after losing access = %v, want none — the title must not leak", got)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE spaces SET private = false WHERE id = $1`, spaceID); err != nil {
		t.Fatalf("make public: %v", err)
	}

	if rr := call(t, chain(d), http.MethodDelete, pinPath, "bob@corp.com"); rr.Code != http.StatusOK {
		t.Fatalf("Bob unpins = %d: %s", rr.Code, rr.Body.String())
	}
	if got := titles(t, call(t, chain(d), http.MethodGet, pins, "bob@corp.com")); len(got) != 0 {
		t.Fatalf("Bob's pins after unpinning = %v, want none", got)
	}

	if _, err := d.Pool.Exec(ctx, `INSERT INTO page_views (page_id, workspace_id, viewer_id) VALUES ($1, $2, $3)`,
		pageID, ws, bob); err != nil {
		t.Fatalf("record a view: %v", err)
	}
	recent := "/v1/workspaces/" + ws + "/recent-pages"
	if got := titles(t, call(t, chain(d), http.MethodGet, recent, "bob@corp.com")); len(got) != 1 || got[0] != "Runbook" {
		t.Fatalf("Bob's recent pages = %v, want [Runbook]", got)
	}
	if got := titles(t, call(t, chain(d), http.MethodGet, recent, "alice@corp.com")); len(got) != 0 {
		t.Fatalf("Alice's recent pages = %v, want none", got)
	}
}
