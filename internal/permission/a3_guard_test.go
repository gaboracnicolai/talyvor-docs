package permission_test

// A3 — intra-workspace resource access control (permission.RequireAccess). All actors are members of
// the SAME workspace W (this is NOT cross-tenant — SEC-4 already closed that). resolveAccess already
// computes the right answer (private space needs a grant; view-tier can't edit); nothing consumed it
// until the guard is mounted. RED (unguarded chain, today): a non-granted member reads a private space,
// a view-tier member edits — the asserts below FAIL. GREEN (guarded chain): the SAME asserts pass.
// Only the chain builder changes red→green; the assertions are the contract.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/block"
	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/sharing"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const a3Secret = "sec4-test-gateway-secret-0123456789"

// a3Chain mounts the resource routes with the A3 access guards, mirroring main.go's wiring: scoped
// resolvers (GetByIDInWorkspaces) + RequireAccess per route. (Pre-fix this builder called .Mount
// without .WithAccess — the RED baseline; this is the GREEN wiring. Only the builder changes red→green.)
func a3Chain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy}, nil
	}
	blockPageLooker := func(ctx context.Context, blockID string) (string, permission.PageMeta, error) {
		var pageID string
		if err := d.Pool.QueryRow(ctx, `SELECT page_id FROM blocks WHERE id=$1`, blockID).Scan(&pageID); err != nil {
			return "", permission.PageMeta{}, err
		}
		md, err := pageLooker(ctx, pageID)
		if err != nil {
			return "", permission.PageMeta{}, err
		}
		return pageID, md, nil
	}
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	blockEnf := permission.NewEnforcer(permStore, permission.PageResolverFromBlock("blockID", blockPageLooker, permStore))

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(a3Secret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		space.NewHandler(spaceStore).WithAccess(spaceEnf).Mount(r)
		page.NewHandler(pageStore, d.Pool).WithAccess(pageEnf, spaceEnf).Mount(r)
		comment.NewHandler(comment.NewStore(d.Pool)).WithAccess(pageEnf).Mount(r)
		sharing.NewHandler(sharing.NewStore(d.Pool), nil).WithAccess(pageEnf).Mount(r)
		block.NewHandler(block.NewStore(d.Pool)).WithAccess(pageEnf, blockEnf).Mount(r)
		export.NewHandler(export.New(pageStore, spaceStore)).WithAccess(pageEnf).Mount(r)
		permission.NewHandler(permStore).WithAccess(spaceEnf, pageEnf).Mount(r)
	})
	return r
}

func a3Req(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", a3Secret)
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

func TestA3_IntraWorkspaceAccessControl(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")         // space creator → admin
	viewer := d.Member(t, W, "viewer@corp.com")       // view grant on P
	d.Member(t, W, "outsider@corp.com")               // member of W, NO grant on the private space
	editor := d.Member(t, W, "editor@corp.com")       // edit grant on P
	commenter := d.Member(t, W, "commenter@corp.com") // comment grant on P — the middle tier
	granted := d.Member(t, W, "granted@corp.com")     // view grant on the private SPACE (inherits to P)

	// A PRIVATE space + a page in it, both created by owner.
	sPriv, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "Private space", Slug: "priv-" + owner[len(owner)-6:], Private: true, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed private space: %v", err)
	}
	p, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sPriv.ID, WorkspaceID: W, Title: "Secret page", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	// A separate page so the DELETE assertion doesn't destroy the page the other assertions use.
	pDel, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sPriv.ID, WorkspaceID: W, Title: "Deletable page", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page 2: %v", err)
	}
	grant := func(rt permission.ResourceType, rid, subject string, lvl permission.AccessLevel) {
		if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: rt, ResourceID: rid, SubjectType: "member", SubjectID: subject,
			Access: lvl, WorkspaceID: W, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(permission.ResourcePage, p.ID, viewer, permission.AccessView)
	grant(permission.ResourcePage, p.ID, editor, permission.AccessEdit)
	grant(permission.ResourcePage, p.ID, commenter, permission.AccessComment)
	grant(permission.ResourceSpace, sPriv.ID, granted, permission.AccessView)

	// A block (page content) on P — for the block-edit hole (edit content bypassing the page guard).
	blk, err := block.NewStore(d.Pool).Create(ctx, model.Block{PageID: p.ID, Type: "paragraph", Content: "secret content"})
	if err != nil {
		t.Fatalf("seed block: %v", err)
	}

	chain := a3Chain(d)
	code := func(r *http.Request) int { rr := httptest.NewRecorder(); chain.ServeHTTP(rr, r); return rr.Code }
	sp := "/v1/spaces/" + sPriv.ID
	pg := sp + "/pages/" + p.ID
	deny := func(c int) bool { return c == http.StatusForbidden } // 403 = authenticated-but-unauthorized

	// (a) Outsider (member of W, NOT granted) reads a PRIVATE space + its page → must be 403.
	if c := code(a3Req(http.MethodGet, sp, "outsider@corp.com", "")); !deny(c) {
		t.Errorf("(a) Outsider GET private space = %d, want 403 (private, no grant)", c)
	}
	if c := code(a3Req(http.MethodGet, pg, "outsider@corp.com", "")); !deny(c) {
		t.Errorf("(a) Outsider GET page in private space = %d, want 403", c)
	}

	// (b) Viewer (view grant) EDITs the page → must be 403 (view < edit).
	if c := code(a3Req(http.MethodPatch, pg, "viewer@corp.com", `{"title":"hacked-by-viewer"}`)); !deny(c) {
		t.Errorf("(b) Viewer PATCH page = %d, want 403 (view-tier cannot edit)", c)
	}

	// (c) Viewer DELETEs a page → must be 403. (Separate page — doesn't affect the others.)
	if c := code(a3Req(http.MethodDelete, sp+"/pages/"+pDel.ID, "viewer@corp.com", "")); !deny(c) {
		t.Errorf("(c) Viewer DELETE page = %d, want 403", c)
	}

	// (d) Outsider does sharing-admin + space-settings → must be 403 (admin).
	if c := code(a3Req(http.MethodPost, pg+"/share", "outsider@corp.com", `{"access":"view"}`)); !deny(c) {
		t.Errorf("(d) Outsider POST share (sharing-admin) = %d, want 403", c)
	}
	if c := code(a3Req(http.MethodPatch, sp, "outsider@corp.com", `{"name":"hijacked"}`)); !deny(c) {
		t.Errorf("(d) Outsider PATCH space settings = %d, want 403", c)
	}

	// (d2) The other resource doors caught by the STEP-4 sweep, all within W:
	// blocks are page CONTENT (a view-tier member must not edit them via /blocks/{blockID}),
	// permission-management must be Admin (a member can't grant itself access), export reads full
	// content (a non-granted member must not export a private page).
	if c := code(a3Req(http.MethodPatch, "/v1/blocks/"+blk.ID, "viewer@corp.com", `{"content":"edited-by-viewer"}`)); !deny(c) {
		t.Errorf("(d2) Viewer PATCH block (content edit via /blocks/{id}) = %d, want 403", c)
	}
	if c := code(a3Req(http.MethodPost, pg+"/permissions", "outsider@corp.com", `{"subject_type":"member","subject_id":"outsider","access":"admin"}`)); !deny(c) {
		t.Errorf("(d2) Outsider POST permission-grant = %d, want 403 (grant/revoke is Admin)", c)
	}
	if c := code(a3Req(http.MethodGet, pg+"/export?format=markdown", "outsider@corp.com", "")); !deny(c) {
		t.Errorf("(d2) Outsider EXPORT private page = %d, want 403", c)
	}

	// (e) DECISION — comment-but-not-edit: the "comment" tier is now REAL (was identical to view).
	// The middle tier must behave differently from BOTH neighbours:
	//   view    → may READ comments but NOT create one (comment participation requires AccessComment);
	//   comment → may create comments but NOT edit the page (AccessComment < AccessEdit);
	//   edit    → may do both (proven by (f) editor PATCH below).
	ok := func(c int) bool { return c == http.StatusOK || c == http.StatusCreated }
	if c := code(a3Req(http.MethodPost, pg+"/comments", "viewer@corp.com", `{"content":"view tries to comment"}`)); !deny(c) {
		t.Errorf("(e) Viewer POST comment = %d, want 403 (view < comment: view is read-only now)", c)
	}
	if c := code(a3Req(http.MethodGet, pg+"/comments", "viewer@corp.com", "")); c != http.StatusOK {
		t.Errorf("(e) Viewer GET comments = %d, want 200 (view can still READ comments)", c)
	}
	if c := code(a3Req(http.MethodPost, pg+"/comments", "commenter@corp.com", `{"content":"a real comment"}`)); !ok(c) {
		t.Errorf("(e) Commenter POST comment = %d, want 200/201 (comment tier CAN comment)", c)
	}
	if c := code(a3Req(http.MethodPatch, pg, "commenter@corp.com", `{"title":"commenter-cannot-edit"}`)); !deny(c) {
		t.Errorf("(e) Commenter PATCH page = %d, want 403 (comment < edit: cannot edit the page)", c)
	}

	// COMPOSITION with SEC-4 L2: a member of a DIFFERENT workspace hitting the guarded route gets 404
	// (the scoped resolver finds nothing in their verified workspaces) — NOT 403. The guard never
	// leaks the existence of an out-of-workspace resource; it composes with the L2 by-id 404s.
	W2 := d.Workspace(t)
	d.Member(t, W2, "mallory@corp.com")
	if c := code(a3Req(http.MethodGet, sp, "mallory@corp.com", "")); c != http.StatusNotFound {
		t.Errorf("cross-tenant GET (other-workspace member) = %d, want 404 (composes with L2, no 403 oracle)", c)
	}
	if c := code(a3Req(http.MethodPatch, pg, "mallory@corp.com", `{"title":"x"}`)); c != http.StatusNotFound {
		t.Errorf("cross-tenant PATCH (other-workspace member) = %d, want 404, not 403", c)
	}

	// (f) POSITIVE controls — the guard must not over-block.
	if c := code(a3Req(http.MethodPatch, pg, "owner@corp.com", `{"title":"owner edit"}`)); c != http.StatusOK {
		t.Errorf("(f) Owner PATCH own page = %d, want 200 (creator=admin)", c)
	}
	if c := code(a3Req(http.MethodPatch, pg, "editor@corp.com", `{"title":"editor edit"}`)); c != http.StatusOK {
		t.Errorf("(f) Editor (edit grant) PATCH page = %d, want 200", c)
	}
	if c := code(a3Req(http.MethodGet, sp, "granted@corp.com", "")); c != http.StatusOK {
		t.Errorf("(f) Granted member GET private space = %d, want 200 (view grant)", c)
	}
	if c := code(a3Req(http.MethodPost, pg+"/share", "owner@corp.com", `{"access":"view"}`)); c != http.StatusOK && c != http.StatusCreated {
		t.Errorf("(f) Owner POST share (admin) = %d, want 200/201", c)
	}
	if c := code(a3Req(http.MethodPatch, "/v1/blocks/"+blk.ID, "owner@corp.com", `{"content":"owner content"}`)); c != http.StatusOK {
		t.Errorf("(f) Owner PATCH block = %d, want 200 (admin can edit content)", c)
	}
	if c := code(a3Req(http.MethodGet, pg+"/export?format=markdown", "owner@corp.com", "")); c != http.StatusOK {
		t.Errorf("(f) Owner EXPORT own page = %d, want 200", c)
	}
}
