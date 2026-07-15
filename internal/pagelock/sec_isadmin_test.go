package pagelock_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// Privilege escalation: Unlock trusted a CLIENT-SUPPLIED `is_admin` from the request
// body to bypass the store's "only the locker or an admin can unlock" rule. Any member
// with Edit access on a page could steal another member's lock by sending
// {"is_admin": true}.
//
// This is NOT cross-tenant — the route gate (pageEnf.Require(AccessEdit)) holds, so a
// foreign page still 404s. It is within-workspace privilege escalation: the caller
// asserts their own privilege level and is believed.
//
// The fix mirrors SEC-4/A3 exactly: admin status comes from the VERIFIED identity
// resolved against the permission model (permission.LevelFromContext, populated by the
// same RequireAccess middleware that already gates the route), never from the body.
//
// RED (pre-fix): Mallory, a non-locker Edit member, steals Bob's lock → 200.
// GREEN (post-fix): 403, the lock survives, and BOTH legitimate paths still work —
// the actual locker unlocks, and a real admin (by verified permission, no body claim)
// unlocks.

const lockTestSecret = "sec4-test-gateway-secret-0123456789"

// v1LockChain mirrors main.go's wiring: gatewayauth + authz, then the pagelock handler
// behind the same pageEnf the real server builds.
func v1LockChain(d *testutil.DB) http.Handler {
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
		return permission.PageMeta{
			SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := pagelock.NewHandler(pagelock.NewStore(d.Pool)).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(lockTestSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func lockReq(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", lockTestSecret)
	r.Header.Set("X-User-Email", email)
	return r
}

func TestSec_Unlock_IgnoresClientSuppliedIsAdmin(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	// Alice creates the space → resolveAccess makes the SPACE CREATOR admin of every
	// page in it. She is the "real admin" by verified permission, not by any claim.
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	mallory := d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Locked spec")

	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}

	// Bob and Mallory are ordinary Edit-tier members — enough to reach the route,
	// nowhere near admin.
	permStore := permission.NewStore(d.Pool)
	for _, m := range []string{bob, mallory} {
		if err := permStore.Grant(ctx, permission.Permission{
			ResourceType: permission.ResourceSpace, ResourceID: spaceID,
			SubjectType: "member", SubjectID: m,
			Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: alice,
		}); err != nil {
			t.Fatalf("grant edit to %s: %v", m, err)
		}
	}

	chain := v1LockChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	lockPath := "/v1/spaces/" + spaceID + "/pages/" + pageID + "/lock"

	lockedBy := func() string {
		t.Helper()
		var by *string
		if err := d.Pool.QueryRow(ctx, `SELECT locked_by FROM pages WHERE id=$1`, pageID).Scan(&by); err != nil {
			t.Fatalf("read locked_by: %v", err)
		}
		if by == nil {
			return ""
		}
		return *by
	}

	// Bob takes the lock.
	if rr := do(lockReq(http.MethodPost, lockPath, "bob@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Fatalf("Bob lock = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != bob {
		t.Fatalf("locked_by = %q, want Bob %q — fixture is wrong", got, bob)
	}

	// (a) THE ESCALATION: Mallory is not the locker and is not an admin. Her body says
	// otherwise. The body must not be believed.
	rr := do(lockReq(http.MethodDelete, lockPath, "mallory@corp.com", `{"is_admin":true}`))
	if rr.Code != http.StatusForbidden {
		t.Errorf("Mallory unlock with {\"is_admin\":true} = %d, want 403 "+
			"(privilege must come from the verified identity, never the body). body=%s",
			rr.Code, rr.Body.String())
	}
	// NO SIDE EFFECT: the lock must still be Bob's.
	if got := lockedBy(); got != bob {
		t.Errorf("after Mallory's blocked unlock, locked_by = %q, want Bob %q — she stole the lock", got, bob)
	}

	// (b) Forging member_id alongside is_admin must not help either.
	rr = do(lockReq(http.MethodDelete, lockPath, "mallory@corp.com",
		`{"is_admin":true,"member_id":"`+bob+`"}`))
	if rr.Code != http.StatusForbidden {
		t.Errorf("Mallory unlock with forged member_id+is_admin = %d, want 403. body=%s", rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != bob {
		t.Errorf("after forged member_id unlock, locked_by = %q, want Bob %q", got, bob)
	}

	// (c) LEGITIMATE PATH 1 — the actual locker unlocks. No body claim needed.
	if rr := do(lockReq(http.MethodDelete, lockPath, "bob@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Errorf("Bob (the locker) unlock = %d, want 200 (the fix must not break the locker path). body=%s",
			rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != "" {
		t.Errorf("after Bob unlocked, locked_by = %q, want empty", got)
	}

	// (d) LEGITIMATE PATH 2 — a REAL admin unlocks someone else's lock, by verified
	// permission (Alice created the space → AccessAdmin on its pages), sending NO
	// is_admin claim at all. This is the capability the body flag was standing in for;
	// it must survive the fix.
	if rr := do(lockReq(http.MethodPost, lockPath, "bob@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Fatalf("Bob re-lock = %d, want 200", rr.Code)
	}
	if rr := do(lockReq(http.MethodDelete, lockPath, "alice@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Errorf("real admin (space creator, no is_admin claim) unlock = %d, want 200 "+
			"(admin override must still work from the VERIFIED permission level). body=%s",
			rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != "" {
		t.Errorf("after admin unlock, locked_by = %q, want empty", got)
	}

	// (e) An admin's is_admin:false must not disarm her real privilege either — the
	// body is ignored in BOTH directions, not merely clamped.
	if rr := do(lockReq(http.MethodPost, lockPath, "bob@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Fatalf("Bob re-lock (e) = %d, want 200", rr.Code)
	}
	if rr := do(lockReq(http.MethodDelete, lockPath, "alice@corp.com", `{"is_admin":false}`)); rr.Code != http.StatusOK {
		t.Errorf("real admin unlock with {\"is_admin\":false} = %d, want 200 (body is ignored entirely)", rr.Code)
	}
}
