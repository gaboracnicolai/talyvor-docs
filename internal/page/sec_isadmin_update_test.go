package page_test

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

// The SAME client-supplied-privilege bug as pagelock's Unlock, in the page EDIT path.
//
// page.Store.Update reads its admin-bypass flag out of the updates map:
//
//	isAdmin, _ := updates["is_admin"].(bool)
//	ok, reason, err := s.guard.CanEdit(ctx, id, memberID, isAdmin)   // CanEdit: if isAdmin → allow
//
// and store.go asserts that flag is "handler-injected, never trusted from request
// bodies". That comment is FALSE. page.Handler.Update does
// `json.NewDecoder(r.Body).Decode(&updates)` — the map IS the request body. It carefully
// overwrites updates["updated_by"] with the verified member id (SEC-4 did that) and
// never touches updates["is_admin"].
//
// So PATCH {"is_admin": true, "title": "..."} bypasses another member's page LOCK.
//
// RED (pre-fix): Mallory edits a page Bob has locked → 200.
// GREEN (post-fix): 423 Locked, Bob's title survives, and both legitimate paths still
// work — the locker edits, and a real admin (verified AccessAdmin, no body claim) edits.
func TestSec_Update_IgnoresClientSuppliedIsAdmin(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com") // space creator → AccessAdmin on its pages
	bob := d.Member(t, ws, "bob@corp.com")
	d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Original title")

	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}

	permStore := permission.NewStore(d.Pool)
	for _, m := range []string{bob, "mallory-placeholder"} {
		_ = m
	}
	// Bob and Mallory are ordinary Edit-tier members.
	for _, email := range []string{"bob@corp.com", "mallory@corp.com"} {
		var mid string
		if err := d.Pool.QueryRow(ctx,
			`SELECT member_id FROM workspace_members WHERE workspace_id=$1 AND email=$2`, ws, email).Scan(&mid); err != nil {
			t.Fatalf("lookup member %s: %v", email, err)
		}
		if err := permStore.Grant(ctx, permission.Permission{
			ResourceType: permission.ResourceSpace, ResourceID: spaceID,
			SubjectType: "member", SubjectID: mid,
			Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: alice,
		}); err != nil {
			t.Fatalf("grant edit to %s: %v", email, err)
		}
	}

	// Chain mirroring main.go: page store WITH the pagelock guard wired (main.go does
	// pageStore = pageStore.WithGuard(lockStore)), behind gatewayauth + authz + pageEnf.
	lockStore := pagelock.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool).WithGuard(lockStore)
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
	pageHandler := page.NewHandler(pageStore, d.Pool)
	pageHandler.WithAccess(pageEnf, pageEnf)
	lockHandler := pagelock.NewHandler(lockStore).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		pageHandler.Mount(r)
		lockHandler.Mount(r)
	})

	do := func(method, path, email, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gateway-Auth", testGatewaySecret)
		req.Header.Set("X-User-Email", email)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	base := "/v1/spaces/" + spaceID + "/pages/" + pageID
	titleNow := func() string {
		t.Helper()
		var title string
		if err := d.Pool.QueryRow(ctx, `SELECT title FROM pages WHERE id=$1`, pageID).Scan(&title); err != nil {
			t.Fatalf("read title: %v", err)
		}
		return title
	}

	// Bob locks the page.
	if rr := do(http.MethodPost, base+"/lock", "bob@corp.com", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("Bob lock = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	// (a) THE BYPASS: Mallory edits Bob's locked page by asserting her own privilege.
	rr := do(http.MethodPatch, base, "mallory@corp.com", `{"is_admin":true,"title":"hacked-by-mallory"}`)
	if rr.Code != http.StatusLocked {
		t.Errorf("Mallory PATCH with {\"is_admin\":true} on Bob's locked page = %d, want 423 "+
			"(admin-bypass must come from the verified identity, never the body). body=%s",
			rr.Code, rr.Body.String())
	}
	if got := titleNow(); got == "hacked-by-mallory" {
		t.Errorf("Mallory's edit LANDED on a page locked by Bob — title is now %q", got)
	}

	// (b) SCOPE, NOT BREAKAGE — Mallory without the flag is still correctly locked out.
	if rr := do(http.MethodPatch, base, "mallory@corp.com", `{"title":"nope"}`); rr.Code != http.StatusLocked {
		t.Errorf("Mallory PATCH without the flag = %d, want 423", rr.Code)
	}

	// (c) LEGITIMATE PATH 1 — the locker edits his own locked page.
	if rr := do(http.MethodPatch, base, "bob@corp.com", `{"title":"bob-edit"}`); rr.Code != http.StatusOK {
		t.Errorf("Bob (the locker) PATCH = %d, want 200 (the fix must not break the locker path). body=%s",
			rr.Code, rr.Body.String())
	}
	if got := titleNow(); got != "bob-edit" {
		t.Errorf("Bob's edit did not land: title = %q", got)
	}

	// (d) LEGITIMATE PATH 2 — a REAL admin edits a page locked by someone else, sending
	// NO is_admin claim. This is the capability the body flag stood in for; it must
	// survive, sourced from the verified permission level.
	if rr := do(http.MethodPatch, base, "alice@corp.com", `{"title":"admin-edit"}`); rr.Code != http.StatusOK {
		t.Errorf("real admin (space creator, no is_admin claim) PATCH on a locked page = %d, want 200 "+
			"(admin override must work from the VERIFIED permission level). body=%s",
			rr.Code, rr.Body.String())
	}
	if got := titleNow(); got != "admin-edit" {
		t.Errorf("real admin's edit did not land: title = %q", got)
	}

	// (e) is_admin must never be persisted as a column, whatever the caller sends.
	if rr := do(http.MethodPatch, base, "alice@corp.com", `{"is_admin":true,"title":"admin-edit-2"}`); rr.Code != http.StatusOK {
		t.Errorf("admin PATCH with a body is_admin = %d, want 200 (flag ignored, edit proceeds on real privilege)", rr.Code)
	}
}
