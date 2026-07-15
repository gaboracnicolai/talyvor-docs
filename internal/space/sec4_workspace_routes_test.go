package space_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// SEC-4 cross-tenant IDOR on the two WORKSPACE-level space routes.
//
// space.Mount's three by-id routes are guarded (spaceEnf.Require + GetByIDInWorkspaces).
// These two are bare:
//
//	GET  /v1/workspaces/{wsID}/spaces  → store.List(ctx, chi.URLParam("wsID"))
//	POST /v1/spaces                    → store.Create(ctx, <model.Space decoded from BODY>)
//
// The POST is the worse of the two: workspace_id AND created_by are caller-supplied, and
// permission/store.go's resolveAccess treats a space's creator as its admin — so a
// forged created_by is plant-and-own on any workspace.
//
// RED (pre-fix): Alice, a member of A only, lists B's spaces and plants an owned space
// in B. GREEN (post-fix): 403 on both, Alice's OWN workspace still works, and created_by
// is the VERIFIED member id regardless of what the body claims.

const spaceTestSecret = "sec4-test-gateway-secret-0123456789"

// v1SpaceChain mirrors main.go's wiring for the space handler: gatewayauth + authz on the
// group, spaceEnf resolving through GetByIDInWorkspaces.
func v1SpaceChain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	h := space.NewHandler(spaceStore)
	h.WithAccess(spaceEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(spaceTestSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func spaceReq(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", spaceTestSecret)
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

func TestSEC4_SpaceWorkspaceRoutes_CrossTenant(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com") // Alice belongs to A only
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com") // Bob belongs to B only

	const secretTitle = "Secret B acquisition roadmap"
	d.Page(t, wsB, bob, secretTitle) // seeds a space named "Space Secret B acquisition roadmap" in B

	chain := v1SpaceChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	// ANCHOR: Bob lists his OWN workspace and actually sees his space. Proves the denials
	// below are scope, not an empty fixture or a broken query.
	if rr := do(spaceReq(http.MethodGet, "/v1/workspaces/"+wsB+"/spaces", "bob@corp.com", "")); rr.Code != http.StatusOK {
		t.Errorf("Bob→B list (own workspace) = %d, want 200 (denial must be scope, not a broken query)", rr.Code)
	} else if !strings.Contains(rr.Body.String(), secretTitle) {
		t.Errorf("Bob→B list returned 200 but not his own space — fixture is wrong, the leak assert below would pass for the wrong reason. body=%s", rr.Body.String())
	}

	// (a) Alice must NOT enumerate B's spaces.
	if rr := do(spaceReq(http.MethodGet, "/v1/workspaces/"+wsB+"/spaces", "alice@corp.com", "")); rr.Code != http.StatusForbidden {
		t.Errorf("Alice→B list = %d, want 403 (cross-tenant space enumeration). body=%s", rr.Code, rr.Body.String())
	} else if strings.Contains(rr.Body.String(), secretTitle) {
		t.Errorf("Alice→B list LEAKED B's space name in a %d response: %s", rr.Code, rr.Body.String())
	}

	// (b) Alice must NOT plant a space in B. workspace_id comes from the body here.
	plant := `{"workspace_id":"` + wsB + `","name":"alice-planted","created_by":"` + bob + `"}`
	rr := do(spaceReq(http.MethodPost, "/v1/spaces", "alice@corp.com", plant))
	if rr.Code != http.StatusForbidden {
		t.Errorf("Alice→POST /v1/spaces into B = %d, want 403 (plant-and-own). body=%s", rr.Code, rr.Body.String())
	}
	// NO SIDE EFFECT: the blocked create must not have landed. A 403 after the row is
	// written would be worse than useless.
	var planted int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM spaces WHERE workspace_id = $1 AND name = 'alice-planted'`, wsB).Scan(&planted); err != nil {
		t.Fatalf("count planted: %v", err)
	}
	if planted != 0 {
		t.Errorf("Alice planted %d space(s) in workspace B — the denial was not a no-op", planted)
	}

	// (c) SCOPE, NOT BREAKAGE: Alice's OWN workspace still works. A blanket-deny guard
	// would pass (a)+(b) while breaking the product.
	own := `{"workspace_id":"` + wsA + `","name":"alice-own-space"}`
	if rr := do(spaceReq(http.MethodPost, "/v1/spaces", "alice@corp.com", own)); rr.Code != http.StatusCreated {
		t.Errorf("Alice→POST into her OWN workspace A = %d, want 201 (the guard must scope, not blanket-deny). body=%s",
			rr.Code, rr.Body.String())
	}
	if rr := do(spaceReq(http.MethodGet, "/v1/workspaces/"+wsA+"/spaces", "alice@corp.com", "")); rr.Code != http.StatusOK {
		t.Errorf("Alice→A list (own workspace) = %d, want 200", rr.Code)
	}

	// (d) created_by FORGERY: even in her own workspace, Alice must not be able to name
	// someone else as creator — resolveAccess makes the creator an admin, so a forged
	// created_by is a privilege grant. It must be the VERIFIED member id.
	forge := `{"workspace_id":"` + wsA + `","name":"alice-forged-creator","created_by":"` + bob + `"}`
	if rr := do(spaceReq(http.MethodPost, "/v1/spaces", "alice@corp.com", forge)); rr.Code != http.StatusCreated {
		t.Fatalf("Alice→POST into own workspace with forged created_by = %d, want 201. body=%s", rr.Code, rr.Body.String())
	}
	var storedCreatedBy string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT created_by FROM spaces WHERE workspace_id = $1 AND name = 'alice-forged-creator'`, wsA).Scan(&storedCreatedBy); err != nil {
		t.Fatalf("read back created_by: %v", err)
	}
	if storedCreatedBy == bob {
		t.Errorf("created_by was taken from the BODY (%q = Bob's member id) — forged creator becomes space admin via permission.resolveAccess", storedCreatedBy)
	}
	if storedCreatedBy != alice {
		t.Errorf("created_by = %q, want Alice's VERIFIED member id %q (attribution must come from the gateway identity, never the body)", storedCreatedBy, alice)
	}

	// (e) TRANSIT PROOF: no x-gateway-auth → 401 before identity is read.
	noProof := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+wsB+"/spaces", nil)
	noProof.Header.Set("X-User-Email", "alice@corp.com")
	if rr := do(noProof); rr.Code != http.StatusUnauthorized {
		t.Errorf("no transit proof = %d, want 401", rr.Code)
	}
}

// A gateway-verified caller with ZERO memberships must not list or create anywhere.
// authz.Middleware proceeds with an empty membership set rather than 401ing, so these
// routes must deny explicitly.
func TestSEC4_SpaceWorkspaceRoutes_ZeroMembershipCallerDenied(t *testing.T) {
	d := testutil.New(t)
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	d.Page(t, wsB, bob, "Secret B acquisition roadmap")

	chain := v1SpaceChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	if rr := do(spaceReq(http.MethodGet, "/v1/workspaces/"+wsB+"/spaces", "nobody@corp.com", "")); rr.Code != http.StatusForbidden {
		t.Errorf("zero-membership caller→B list = %d, want 403. body=%s", rr.Code, rr.Body.String())
	}
	plant := `{"workspace_id":"` + wsB + `","name":"nobody-planted"}`
	if rr := do(spaceReq(http.MethodPost, "/v1/spaces", "nobody@corp.com", plant)); rr.Code != http.StatusForbidden {
		t.Errorf("zero-membership caller→POST into B = %d, want 403. body=%s", rr.Code, rr.Body.String())
	}
	var planted int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM spaces WHERE workspace_id = $1 AND name = 'nobody-planted'`, wsB).Scan(&planted); err != nil {
		t.Fatalf("count planted: %v", err)
	}
	if planted != 0 {
		t.Errorf("zero-membership caller planted %d space(s) in B", planted)
	}
}
