package permission_test

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
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// THE ROOT of the client-supplied-authority class.
//
// permission.RequireAccess resolves the acting member with authz.ActorOrEmpty →
// SingleMemberID, which returns "" for ANY caller whose membership count != 1. So a
// member of TWO workspaces is evaluated as memberID "" against the permission model:
// resolveAccess skips the creator rule (guarded on memberID != ""), and no
// subject_type='member' grant can ever match "". They silently collapse to whatever
// `everyone` grants / public-space defaults allow.
//
// Two consequences, and they pull in opposite directions — which is why the fix must be
// a per-resource-workspace actor, not "delete the body fallbacks":
//
//   FUNCTIONAL (this file): a multi-workspace member's legitimate per-member grant does
//   not work. Deleting the fallbacks would harden this into a permanent lockout.
//
//   SECURITY (sec_root_actor_test.go in pagelock): because ActorOrEmpty is "", the
//   `memberFromReq(r, in.MemberID)` fallbacks go live and the request BODY becomes the
//   caller's identity.
//
// The fix threads the resource's workspace into resourceContext and resolves the actor
// with authz.MemberIDForWorkspace(ctx, res.WorkspaceID) — the caller's real member id IN
// the workspace that owns the resource, from the verified identity.
//
// RED: Mallory (2 workspaces) is denied despite an explicit Edit grant, while Bob (1
// workspace) with the IDENTICAL grant is allowed — proving the denial is the membership
// count, not the grant.
// GREEN: both allowed; Mallory resolves to her real member id in the page's workspace.

const rootSecret = "sec4-test-gateway-secret-0123456789"

func rootChain(d *testutil.DB) http.Handler {
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
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := page.NewHandler(pageStore, d.Pool)
	h.WithAccess(pageEnf, pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(rootSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func rootReq(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", rootSecret)
	r.Header.Set("X-User-Email", email)
	return r
}

func TestRoot_MultiWorkspaceMember_PerMemberGrantIsHonoured(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)

	alice := d.Member(t, wsA, "alice@corp.com") // space creator → admin
	bob := d.Member(t, wsA, "bob@corp.com")     // ONE workspace — the control
	// Mallory is a member of BOTH workspaces → SingleMemberID returns "" for her.
	mallory := d.Member(t, wsA, "mallory@corp.com")
	d.Member(t, wsB, "mallory@corp.com")

	pageID := d.Page(t, wsA, alice, "Spec")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}
	// Make the space PRIVATE so the public-space AccessView default cannot mask the
	// result: access here comes from the explicit per-member grant or nowhere.
	if _, err := d.Pool.Exec(ctx, `UPDATE spaces SET private = true WHERE id=$1`, spaceID); err != nil {
		t.Fatalf("make space private: %v", err)
	}

	// IDENTICAL explicit Edit grants for Bob (1 ws) and Mallory (2 ws).
	permStore := permission.NewStore(d.Pool)
	for _, m := range []string{bob, mallory} {
		if err := permStore.Grant(ctx, permission.Permission{
			ResourceType: permission.ResourceSpace, ResourceID: spaceID,
			SubjectType: "member", SubjectID: m,
			Access: permission.AccessEdit, WorkspaceID: wsA, GrantedBy: alice,
		}); err != nil {
			t.Fatalf("grant edit to %s: %v", m, err)
		}
	}

	chain := rootChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	base := "/v1/spaces/" + spaceID + "/pages/" + pageID

	// CONTROL: Bob, one workspace, identical grant → allowed. Anchors that the grant
	// itself is correct, so Mallory's denial below is the membership COUNT, not the grant.
	if rr := do(rootReq(http.MethodPatch, base, "bob@corp.com", `{"title":"bob-edit"}`)); rr.Code != http.StatusOK {
		t.Fatalf("CONTROL Bob (1 workspace, explicit Edit grant) PATCH = %d, want 200 — "+
			"the fixture is wrong and the assertion below would pass for the wrong reason. body=%s",
			rr.Code, rr.Body.String())
	}

	// THE ROOT: Mallory has the SAME grant and differs only by belonging to a second,
	// unrelated workspace.
	rr := do(rootReq(http.MethodPatch, base, "mallory@corp.com", `{"title":"mallory-edit"}`))
	if rr.Code != http.StatusOK {
		t.Errorf("Mallory (2 workspaces, IDENTICAL explicit Edit grant) PATCH = %d, want 200. "+
			"authz.ActorOrEmpty returns \"\" for any caller with != 1 memberships, so "+
			"RequireAccess evaluated her as memberID \"\" and her per-member grant could not "+
			"match. body=%s", rr.Code, rr.Body.String())
	}

	// The edit must actually have landed, attributed to HER real member id in wsA.
	var title, updatedBy string
	if err := d.Pool.QueryRow(ctx, `SELECT title, updated_by FROM pages WHERE id=$1`, pageID).Scan(&title, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if title != "mallory-edit" {
		t.Errorf("title = %q, want %q — Mallory's edit did not land", title, "mallory-edit")
	}
	if updatedBy != mallory {
		t.Errorf("updated_by = %q, want Mallory's member id in wsA %q (the actor must resolve "+
			"per-resource-workspace, not collapse to \"\")", updatedBy, mallory)
	}
}

// A member of two workspaces must still be denied on a resource in a workspace where
// they hold no grant — the fix resolves the actor, it does not widen access.
func TestRoot_MultiWorkspaceMember_NoGrantStillDenied(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	d.Member(t, wsA, "mallory@corp.com")
	d.Member(t, wsB, "mallory@corp.com")

	pageID := d.Page(t, wsA, alice, "Private spec")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE spaces SET private = true WHERE id=$1`, spaceID); err != nil {
		t.Fatal(err)
	}

	chain := rootChain(d)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, rootReq(http.MethodPatch,
		"/v1/spaces/"+spaceID+"/pages/"+pageID, "mallory@corp.com", `{"title":"nope"}`))

	if rr.Code != http.StatusForbidden {
		t.Errorf("multi-workspace member with NO grant on a private space = %d, want 403 "+
			"(resolving the actor must not widen access). body=%s", rr.Code, rr.Body.String())
	}
}
