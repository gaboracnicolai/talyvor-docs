package permission_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// THE ONE PERMISSION ROUTE SHARED BY TWO ADDRESSES WAS THE ONE THAT DID NOT NAME ITS RESOURCE.
//
// Five of the six permission-management routes derive (resource_type, resource_id) from the URL —
// listSpace/listPage pass ResourceSpace+{spaceID} / ResourcePage+{pageID} to ListForResource, and
// grantSpace/grantPage pass the same pair to Grant. The sixth is `h.delete`, and it is the only
// handler MOUNTED TWICE:
//
//	DELETE /spaces/{spaceID}/permissions/{permID}                  spaceEnf, Admin
//	DELETE /spaces/{spaceID}/pages/{pageID}/permissions/{permID}   pageEnf,  Admin
//
// It read only {permID}, and RevokeByID scoped by `workspace_id = ANY($2)`. So the enforcer
// authorized one resource and the DELETE named another, with only the WORKSPACE between them.
//
// ⚠ MEASURED THROUGH THE REAL ROUTES ON REAL POSTGRES BEFORE ANY CHANGE. alice grants carol `edit`
// on her PRIVATE space. bob is a plain workspace member with no grant on it, who has created a
// space of his own (so resolveAccess's owner-is-admin arm makes him Admin THERE, and `POST /spaces`
// is registered with no enforcer at all, so that step needs nobody's permission):
//
//	bob GET    /spaces/{alicePrivate}/permissions            -> 403 forbidden
//	bob DELETE /spaces/{bobSpace}/permissions/{carolsGrant}   -> 200 {"ok":true}, row count 0
//
// He cannot LIST the grants on that space and he revoked one. Reaching it needs the grant's id,
// which is not an oracle this route hands out — but ids leak (a URL, a log line, an export, an
// admin screenshot), and "you must already know the id" is the property every by-id tenancy fix in
// this repository has declined to rely on.
//
// ⚠ IT ONLY REVOKES, AND SAYING SO MATTERS: resolveAccess has no DENY — a grant only RAISES — so
// this is a destructive write on the access-control table, not an escalation. What it destroys is
// somebody else's access to a space the caller cannot see.
//
// ⚠⚠ THE DOC-COMMENT ON RevokeByID REASONS ABOUT THE WRONG RING, AT LENGTH: "This is the worst op
// to leave unscoped, since a cross-tenant caller could otherwise revoke any grant (incl. admin) in
// a workspace they don't belong to." That is the CROSS-TENANT ring, and it was closed. The
// within-workspace ring — one member's space against another's — was never named, and
// internal/database/sec4_l2_test.go (b) is precisely the outer-ring test: it drives THIS route,
// asserts 404 for a foreign tenant, and is green throughout the leak measured above. A guard at
// the outer boundary reads as coverage for the inner one.
//
// ⚠ .semgrep/operate-by-id-tenancy.yml cannot see it either: its predicate is `workspace_id = ANY`
// and this statement has it. The rule is satisfied by the very predicate that is the defect.
//
// This is finding (18)'s first copy; `a86417b` (#82) closed the changelog copy of the same seam.
func TestPermissionRevoke_MustNameTheResourceTheRouteAuthorized_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")
	carol := d.Member(t, ws, "carol@example.com")
	dave := d.Member(t, ws, "dave@example.com")

	privSpace := seedSpaceR(t, d, ws, alice, "Board Private", true)
	privPage := seedPageR(t, d, ws, privSpace, alice, "Board Memo")
	bobSpace := seedSpaceR(t, d, ws, bob, "Bobs Own", false)
	bobPage := seedPageR(t, d, ws, bobSpace, bob, "Bobs Page")

	// The four grants: two on alice's private resources (what must survive), two on bob's own
	// (what must still be revocable, so the fix cannot be "refuse everything").
	aliceSpaceGrant := seedGrantR(t, d, ws, "space", privSpace, carol, "edit")
	// A SECOND grant on alice's private space, used ONLY by P-HONEST-DELETE, so a refusal at the
	// honest address and a refusal of the borrowed one are never the same row.
	aliceSpaceGrant2 := seedGrantR(t, d, ws, "space", privSpace, dave, "edit")
	alicePageGrant := seedGrantR(t, d, ws, "page", privPage, carol, "edit")
	bobSpaceGrant := seedGrantR(t, d, ws, "space", bobSpace, carol, "view")
	bobPageGrant := seedGrantR(t, d, ws, "page", bobPage, carol, "view")
	// ⚠ P-TYPE'S SUBJECT IS MANUFACTURED, AND THE FIRST VERSION OF THIS GUARD COULD NOT SEE ITS OWN
	// PREDICATE. P-TYPE was written against an ordinary grant on bob's PAGE, and control C2 (the
	// resource_type predicate dropped, resource_id kept) fired NOTHING: a page id never equals a
	// space id, so the id half discriminates on its own and the type half was unfalsifiable.
	//
	// That is a property of `gen_random_uuid()`, not of the schema. `permissions.resource_id` has NO
	// foreign key and the table's own uniqueness key is UNIQUE (resource_type, resource_id,
	// subject_type, subject_id) — the PAIR is the resource identity here, so scoping a by-id delete
	// to the id alone is scoping to half a key. This row is that pair colliding: a PAGE grant whose
	// resource_id is bob's SPACE id. It is inert for access resolution (resolveAccess reads space
	// grants as (space, id) and page grants as (page, id), so nothing resolves it) and it is
	// deletable, which is the whole point.
	collidingGrant := seedGrantR(t, d, ws, "page", bobSpace, dave, "view")

	store := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	// cmd/docs/main.go's lookers and enforcers, re-derived rather than borrowed.
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
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID,
			SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	spaceEnf := permission.NewEnforcer(store, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(store, permission.PageResolverFromParam("pageID", pageLooker, store))

	do := func(email, memberID, method, path string) (int, string) {
		t.Helper()
		h := permission.NewHandler(store).WithAccess(spaceEnf, pageEnf)
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		req := httptest.NewRequest(method, path, strings.NewReader(""))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}

	// ── P-PREMISE. bob's Admin on his OWN space is real and the route serves. Without it every
	// refusal below would be a refusal of a route that never worked. Instrument check.
	if code, body := do("bob@example.com", bob, http.MethodGet, "/v1/spaces/"+bobSpace+"/permissions"); code != http.StatusOK {
		t.Fatalf("[P-PREMISE] PREMISE FAILED: bob cannot list the grants on HIS OWN space (%d %s) — "+
			"he is not Admin anywhere, so nothing below would mean anything", code, body)
	}

	// ── P-HONEST. The gate at the private space's own address. This is what makes the leak a
	// statement about the PAIR: the product already knows bob may not touch this resource.
	if code, _ := do("bob@example.com", bob, http.MethodGet, "/v1/spaces/"+privSpace+"/permissions"); code == http.StatusOK {
		t.Errorf("[P-HONEST] bob listed the PRIVATE space's grants at its own address (200) — the " +
			"space enforcer is not refusing, so the leak below is a broken gate and the fix " +
			"belongs somewhere else")
	}

	// ── P-HONEST-DELETE. The gate on the WRITE at the private space's own address. P-HONEST is a
	// read; this is what a deleted `.With(spaceEnf.Require(AccessAdmin))` moves, and nothing else
	// in this guard can see that — the store scope would still be satisfied by the honest URL.
	if code, body := do("bob@example.com", bob, http.MethodDelete, "/v1/spaces/"+privSpace+"/permissions/"+aliceSpaceGrant2); code == http.StatusOK || !existsR(t, d, aliceSpaceGrant2) {
		t.Errorf("[P-HONEST-DELETE] bob revoked a grant on the PRIVATE space at its OWN address "+
			"(%d %s) — the Admin gate on the delete route is not refusing", code, body)
	}

	// ── P-LEAK-SPACE. THE DEFECT, on the space route.
	code, body := do("bob@example.com", bob, http.MethodDelete, "/v1/spaces/"+bobSpace+"/permissions/"+aliceSpaceGrant)
	if existsR(t, d, aliceSpaceGrant) == false {
		t.Errorf("[P-LEAK-SPACE] LEAK: bob revoked carol's grant on ALICE'S PRIVATE SPACE by naming "+
			"HIS OWN space in the URL (%d %s) — the row is gone, and bob cannot even list that "+
			"space's grants", code, body)
	}

	// ── P-LEAK-PAGE. The same seam on the page route — a different enforcer, the same handler.
	code, body = do("bob@example.com", bob, http.MethodDelete,
		"/v1/spaces/"+bobSpace+"/pages/"+bobPage+"/permissions/"+alicePageGrant)
	if existsR(t, d, alicePageGrant) == false {
		t.Errorf("[P-LEAK-PAGE] LEAK: bob revoked carol's grant on a PAGE IN ALICE'S PRIVATE SPACE by "+
			"naming HIS OWN page in the URL (%d %s) — the row is gone", code, body)
	}

	// ── P-TYPE. The two routes are the same handler, so the resource TYPE has to be part of the
	// scope and not only the id: the space route must not reach a PAGE grant even when the caller
	// admins both. Asserted on bob's OWN page grant, so it is about the type and nothing else.
	code, body = do("bob@example.com", bob, http.MethodDelete, "/v1/spaces/"+bobSpace+"/permissions/"+collidingGrant)
	if existsR(t, d, collidingGrant) == false {
		t.Errorf("[P-TYPE] the SPACE route deleted a PAGE grant whose resource_id is this space's id "+
			"(%d %s) — resource_id alone is half the table's own key, and the two routes are one "+
			"handler", code, body)
	}

	// ── P-OWN-PAGE. Over-correction: the page route must still revoke a grant on the page it names.
	if code, body := do("bob@example.com", bob, http.MethodDelete,
		"/v1/spaces/"+bobSpace+"/pages/"+bobPage+"/permissions/"+bobPageGrant); code != http.StatusOK {
		t.Errorf("[P-OWN-PAGE] OVER-CORRECTION: bob cannot revoke a grant on HIS OWN page (%d %s)", code, body)
	} else if existsR(t, d, bobPageGrant) {
		t.Errorf("[P-OWN-PAGE] the route answered 200 and the grant is still on disk")
	}

	// ── P-OWN-SPACE. The same in the space direction. Last, so P-TYPE above still had its subject.
	if code, body := do("bob@example.com", bob, http.MethodDelete,
		"/v1/spaces/"+bobSpace+"/permissions/"+bobSpaceGrant); code != http.StatusOK {
		t.Errorf("[P-OWN-SPACE] OVER-CORRECTION: bob cannot revoke a grant on HIS OWN space (%d %s)", code, body)
	} else if existsR(t, d, bobSpaceGrant) {
		t.Errorf("[P-OWN-SPACE] the route answered 200 and the grant is still on disk")
	}
}

func seedSpaceR(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator, private).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageR(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1,$2,$3,$4,$5,'body','{"type":"doc","content":[]}') RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// seedGrantR writes the row directly rather than through Grant: a fixture built by the code under
// test moves when that code moves.
func seedGrantR(t *testing.T, d *testutil.DB, wsID, resType, resID, subject, access string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ($1,$2,'member',$3,$4,$5) RETURNING id`,
		resType, resID, subject, access, wsID).Scan(&id); err != nil {
		t.Fatalf("seed %s grant: %v", resType, err)
	}
	return id
}

// existsR reads the row straight from Postgres — never through ListForResource, which the same
// edit could move.
func existsR(t *testing.T, d *testutil.DB, id string) bool {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM permissions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count permissions %s: %v", id, err)
	}
	return n == 1
}
