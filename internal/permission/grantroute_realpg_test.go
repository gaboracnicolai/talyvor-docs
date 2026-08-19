package permission_test

// THE ROUTE THAT MINTS A GRANT ANSWERED 201 WITH AN EMPTY PRIMARY KEY, AND NOTHING HAD EVER RUN IT.
//
// ⚠⚠ MEASURED FIRST, ON THE UNMODIFIED TREE AT main `74effc8a`, THROUGH THE REAL CHAIN ON REAL
// POSTGRES — the whole-tree coverage census (`go test -coverprofile -coverpkg=./... ./...`, then
// `go tool cover -func | awk '$3=="0.0%"'`) put THREE functions of the authz write path at 0.0%:
//
//	internal/permission/handler.go:83  grantSpace  0.0%
//	internal/permission/handler.go:86  grantPage   0.0%
//	internal/permission/handler.go:90  grant       0.0%
//
// Driven for the first time, the space route answered:
//
//	201 {"id":"","resource_type":"space","resource_id":"7f59…","subject_type":"member",
//	     "subject_id":"mbr_449c…","access":"edit","workspace_id":"ws_319f…",
//	     "granted_by":"mbr_a732…","created_at":"0001-01-01T00:00:00Z"}
//
// while GET on the same resource returned the same grant as
// `{"id":"e528ad57-09ab-43e5-8b3a-a5704981b2ff", …,"created_at":"2026-08-19T09:56:57+03:00"}`.
//
// `id` and `created_at` are STRUCTURALLY EMPTY on every 201, in both directions of the route pair,
// because the handler serialized the `Permission` IT BUILT rather than the row Postgres wrote —
// `Store.Grant` returned only `error`, so the id `gen_random_uuid()` assigned and the `created_at`
// the column defaulted never came back up.
//
// ⚠ WHY THE EMPTY FIELD IS THE ONE THAT MATTERS: `id` is the key the SIBLING ROUTE TAKES.
// `DELETE /v1/spaces/{spaceID}/permissions/{permID}` addresses a grant by exactly this id, and the
// create route advertises the full `Permission` shape — the frontend's `permissionsApi.grantPage`
// is even typed `Promise<Permission>` with `id: string` — so the response is a contract that says
// "here is the row, and here is its key", and the key is "". Anyone who wires
// revoke(create.id) revokes nothing.
//
// ⚠ STATED PLAINLY, NOT INFLATED: NOTHING IS BROKEN ON A SCREEN TODAY. `SharePanel.tsx` throws the
// create response away and refetches the list, so it never reads the empty key. This is the trap
// the next caller falls into, the same class as the `TrackApiError`/`useTrackProbe` findings —
// and it is invisible to review precisely because no test had ever looked at this body.
//
// ⚠ WHAT ELSE THIS GUARD PINS, AND IT IS NOT PART OF THE FIX: driving the route also executes the
// SEC-4 workspace stamp and the actor stamp for the first time. Both were already CORRECT — the
// probe sent `"workspace_id":"forged-ws"` and the row was written into the caller's real workspace.
// They are asserted here because a route with a test is the only place that stays true, and the
// controls (C5, C6) show both assertions are live rather than decorative.
//
// ⚠ RED-FIRST: every assertion below FAILED on the unmodified tree except [WS-STAMP], [ACTOR-STAMP]
// and [SUBJECT-KEPT], which are the pins on already-correct behaviour and are earned by their
// mutations instead. Harness: scripts/w31-grant-route-controls-9d47.py.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// grantFixture stands up the real chain: the real stores, cmd/docs/main.go's lookers and enforcers
// re-derived rather than borrowed (a fixture that imports the wiring moves when the wiring moves),
// and a router that supplies the gateway-verified membership the way the gateway does.
type grantFixture struct {
	d      *testutil.DB
	ws     string
	alice  string // creator of the space+page → Admin by resolveAccess's owner-is-admin arm
	carol  string // the grant subject
	space  string
	page   string
	do     func(email, memberID, method, path, body string) (int, string)
	router func() http.Handler
}

func newGrantFixture(t *testing.T) *grantFixture {
	t.Helper()
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	carol := d.Member(t, ws, "carol@example.com")

	sp := seedSpaceR(t, d, ws, alice, "Grant Route Space", true)
	pg := seedPageR(t, d, ws, sp, alice, "Grant Route Page")

	store := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		s, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: s.WorkspaceID, Private: s.Private, CreatedBy: s.CreatedBy}, nil
	}
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		p, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		s, err := spaceStore.GetByIDInWorkspaces(ctx, p.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: p.WorkspaceID, SpaceID: p.SpaceID,
			SpaceCreatedBy: s.CreatedBy, SpacePrivate: s.Private, PageCreatedBy: p.CreatedBy,
		}, nil
	}
	spaceEnf := permission.NewEnforcer(store, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(store, permission.PageResolverFromParam("pageID", pageLooker, store))

	f := &grantFixture{d: d, ws: ws, alice: alice, carol: carol, space: sp, page: pg}
	f.router = func() http.Handler {
		h := permission.NewHandler(store).WithAccess(spaceEnf, pageEnf)
		r := chi.NewRouter()
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		return r
	}
	f.do = func(email, memberID, method, path, body string) (int, string) {
		t.Helper()
		inner := f.router()
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Mount("/", inner)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	return f
}

// decodeGrant parses a 201 body into the wire shape. It decodes into a map as well as the struct so
// a MISSING key and an EMPTY one are distinguishable: `id` absent from the JSON is a different
// (and also wrong) contract from `id: ""`, and a struct decode alone reads them identically.
func decodeGrant(t *testing.T, body string) (permission.Permission, map[string]any) {
	t.Helper()
	var p permission.Permission
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("create response is not a Permission: %v — body was %s", err, body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("create response is not an object: %v", err)
	}
	return p, raw
}

// listGrants reads the sibling GET route — the surface whose body the create route claims to
// mirror. Read through the ROUTE and not the store, because "the two routes agree" is the
// statement, and a store read would agree with itself.
func (f *grantFixture) listGrants(t *testing.T, path string) []permission.Permission {
	t.Helper()
	code, body := f.do("alice@example.com", f.alice, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("[SETUP] the LIST route did not serve (%d %s) — nothing below can be compared "+
			"against it", code, body)
	}
	var out []permission.Permission
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("[SETUP] list body is not []Permission: %v — %s", err, body)
	}
	return out
}

// rowExists reads Postgres directly. Never through ListForResource: the same edit that breaks the
// route could move the reader.
func (f *grantFixture) rowExists(t *testing.T, id string) bool {
	t.Helper()
	if id == "" {
		return false // an empty key addresses no row; say so rather than matching everything
	}
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM permissions WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count permissions %q: %v", id, err)
	}
	return n == 1
}

// TestGrantRoute_CreateReturnsThePersistedRow_RealPG is the guard the three 0.0%s left uncovered:
// what the create route SAYS it made.
func TestGrantRoute_CreateReturnsThePersistedRow_RealPG(t *testing.T) {
	f := newGrantFixture(t)

	// ── P-PREMISE. The route serves at all, for the caller this test uses. Without this every
	// assertion below could be satisfied by a route that refuses everything.
	code, body := f.do("alice@example.com", f.alice, http.MethodPost, "/v1/spaces/"+f.space+"/permissions",
		`{"subject_type":"member","subject_id":"`+f.carol+`","access":"edit","workspace_id":"forged-workspace"}`)
	if code != http.StatusCreated {
		t.Fatalf("[P-PREMISE] the space grant route did not answer 201 (%d %s) — alice creates the "+
			"space, so resolveAccess's owner-is-admin arm makes her Admin on it", code, body)
	}
	created, raw := decodeGrant(t, body)

	// ⚠ THE STAMP ASSERTIONS COME FIRST, AND CONTROL C6 IS WHY. They were originally written below
	// the [LIST-NONEMPTY] floor, and C6 — the SEC-4 workspace override weakened to
	// `if in.WorkspaceID == ""` — never reached them: the forged workspace_id makes the row
	// invisible to the workspace-scoped LIST, so the floor Fatalf'd first and [WS-STAMP] printed
	// nothing. The mutation WAS caught, by the wrong assertion, and a reader of that output would
	// have concluded the tenancy pin was live when it was unreachable. These read only the create
	// body, so they need no list and now run before anything that does.

	// ── WS-STAMP. Already correct before this change; pinned because the route had no test at all.
	// The request above sent `"workspace_id":"forged-workspace"`; SEC-4 overrides it from the
	// workspace the enforcer resolved off the RESOURCE.
	if created.WorkspaceID != f.ws {
		t.Errorf("[WS-STAMP] the grant was written with workspace_id=%q, not the resource's %q — a "+
			"client-supplied workspace_id reached the row", created.WorkspaceID, f.ws)
	}
	if created.WorkspaceID == "forged-workspace" {
		t.Errorf("[WS-STAMP] the BODY's workspace_id was persisted verbatim")
	}

	// ── ACTOR-STAMP. Same shape on granted_by: the acting member comes from the verified
	// membership, never from anything the client sent.
	if created.GrantedBy != f.alice {
		t.Errorf("[ACTOR-STAMP] granted_by=%q, expected the caller's member id %q",
			created.GrantedBy, f.alice)
	}

	// ── SUBJECT-KEPT. The over-correction guard: deriving the tenancy fields must not also
	// overwrite the fields the caller legitimately chose.
	if created.SubjectID != f.carol || created.SubjectType != "member" || created.Access != "edit" ||
		created.ResourceType != "space" || created.ResourceID != f.space {
		t.Errorf("[SUBJECT-KEPT] the response does not describe the grant that was asked for: %+v", created)
	}

	// ── LIVENESS FLOOR. Every agreement assertion below compares the create body against the list;
	// with an empty list they would agree vacuously. This fails the run before that can happen.
	listed := f.listGrants(t, "/v1/spaces/"+f.space+"/permissions")
	if len(listed) == 0 {
		t.Fatalf("[LIST-NONEMPTY] the LIST route returned no grants after a 201 — the comparisons "+
			"below would pass on an empty set, which is why this floor exists (created: %s)", body)
	}

	// ── ID-USABLE. THE DEFECT. The response advertises a Permission and its key was "".
	if _, present := raw["id"]; !present {
		t.Errorf("[ID-USABLE] the create response has no `id` key at all — the frontend types this " +
			"route as Promise<Permission> and DELETE /permissions/{permID} takes exactly this field")
	}
	if created.ID == "" {
		t.Errorf("[ID-USABLE] the create route answered 201 with an EMPTY id: %s\n"+
			"the grant IS on disk (the list route returns it with a real uuid) — the handler "+
			"serialized the struct it built instead of the row Postgres wrote", body)
	}

	// ── CREATED-AT. The same emptiness, on the field a UI orders grants by (ListForResource sorts
	// by created_at ASC "so the UI shows the original owner first").
	if created.CreatedAt.IsZero() {
		t.Errorf("[CREATED-AT] the create route answered 201 with a zero created_at (%q) — the row's "+
			"column default is what the list route reports", created.CreatedAt)
	}

	// ── LIST-AGREES. Pins the id to THE ROW rather than to "some uuid": a fix that invented an id
	// client-side, or returned a different grant's, passes ID-USABLE and fails here.
	var match *permission.Permission
	for i := range listed {
		if listed[i].SubjectID == f.carol && listed[i].ResourceID == f.space {
			match = &listed[i]
			break
		}
	}
	if match == nil {
		t.Fatalf("[LIST-AGREES] the grant just created is not in the LIST route's output — the two "+
			"routes are reading different things (list: %+v)", listed)
	}
	if created.ID != match.ID {
		t.Errorf("[LIST-AGREES] create said id=%q, list says id=%q for the same (resource, subject) — "+
			"the create body is not the persisted row", created.ID, match.ID)
	}
	if !created.CreatedAt.Equal(match.CreatedAt) {
		t.Errorf("[LIST-AGREES] create said created_at=%v, list says %v", created.CreatedAt, match.CreatedAt)
	}

	// ── ROUNDTRIP-REVOKE. THE PRODUCT STATEMENT, and the reason this file is not a field-shape
	// test. Take the id the create route handed back and give it to the sibling DELETE route — the
	// exact sequence a caller writes. On the unmodified tree this is DELETE …/permissions/ with an
	// empty id, which chi does not even route to the handler.
	if !f.rowExists(t, created.ID) {
		t.Fatalf("[ROUNDTRIP-REVOKE] the id from the create response (%q) addresses no row, so the "+
			"revoke below would be meaningless", created.ID)
	}
	rc, rb := f.do("alice@example.com", f.alice, http.MethodDelete,
		"/v1/spaces/"+f.space+"/permissions/"+created.ID, "")
	if rc != http.StatusOK {
		t.Errorf("[ROUNDTRIP-REVOKE] revoking the grant by the id its own create route returned "+
			"answered %d %s", rc, rb)
	}
	if f.rowExists(t, created.ID) {
		t.Errorf("[ROUNDTRIP-REVOKE] the revoke answered %d and the row is still on disk", rc)
	}
}

// TestGrantRoute_PageTierReturnsThePersistedRow_RealPG is the same assertion at the OTHER address.
// grantSpace and grantPage are separate 0.0% functions behind separate enforcers; a fix applied to
// one handler and not the other is exactly what two entries in a coverage census mean.
func TestGrantRoute_PageTierReturnsThePersistedRow_RealPG(t *testing.T) {
	f := newGrantFixture(t)

	base := "/v1/spaces/" + f.space + "/pages/" + f.page + "/permissions"
	code, body := f.do("alice@example.com", f.alice, http.MethodPost, base,
		`{"subject_type":"member","subject_id":"`+f.carol+`","access":"view"}`)
	if code != http.StatusCreated {
		t.Fatalf("[P-PREMISE-PAGE] the page grant route did not answer 201 (%d %s)", code, body)
	}
	created, _ := decodeGrant(t, body)

	if created.ID == "" {
		t.Errorf("[ID-USABLE-PAGE] the page-tier create route answered 201 with an empty id: %s", body)
	}
	if created.CreatedAt.IsZero() {
		t.Errorf("[CREATED-AT-PAGE] the page-tier create route answered 201 with a zero created_at")
	}
	if created.ResourceType != "page" || created.ResourceID != f.page {
		t.Errorf("[PAGE-RESOURCE] the page route described resource (%s,%s), expected (page,%s)",
			created.ResourceType, created.ResourceID, f.page)
	}
	if !f.rowExists(t, created.ID) {
		t.Fatalf("[ROUNDTRIP-REVOKE-PAGE] the id from the page create response (%q) addresses no row",
			created.ID)
	}
	rc, rb := f.do("alice@example.com", f.alice, http.MethodDelete, base+"/"+created.ID, "")
	if rc != http.StatusOK || f.rowExists(t, created.ID) {
		t.Errorf("[ROUNDTRIP-REVOKE-PAGE] revoking by the id the page create route returned: %d %s "+
			"(row still present: %v)", rc, rb, f.rowExists(t, created.ID))
	}
}

// TestGrantRoute_RegrantIsAnUpgradeAndKeepsItsID_RealPG covers the ON CONFLICT arm.
//
// ⚠ THIS IS THE ARM A NAIVE FIX BREAKS. `Grant`'s statement is INSERT … ON CONFLICT (resource,
// subject) DO UPDATE — its own doc-comment says re-granting is "an access upgrade rather than a
// duplicate row". Anyone adding RETURNING by rewriting the statement as a plain INSERT gets a
// duplicate-key error on the second grant; anyone switching DO UPDATE to DO NOTHING gets
// pgx.ErrNoRows and a 400 on a request that used to succeed. Neither shows up in the first two
// tests, which only ever grant once.
func TestGrantRoute_RegrantIsAnUpgradeAndKeepsItsID_RealPG(t *testing.T) {
	f := newGrantFixture(t)
	path := "/v1/spaces/" + f.space + "/permissions"
	body := func(access string) string {
		return `{"subject_type":"member","subject_id":"` + f.carol + `","access":"` + access + `"}`
	}

	code, b1 := f.do("alice@example.com", f.alice, http.MethodPost, path, body("view"))
	if code != http.StatusCreated {
		t.Fatalf("[P-PREMISE-REGRANT] first grant did not answer 201 (%d %s)", code, b1)
	}
	first, _ := decodeGrant(t, b1)

	// A second grant of the same (resource, subject) at a higher tier.
	code, b2 := f.do("alice@example.com", f.alice, http.MethodPost, path, body("admin"))
	if code != http.StatusCreated {
		t.Fatalf("[UPSERT-STILL-SERVES] re-granting the same subject answered %d %s — the route's "+
			"own contract is that this is an upgrade, not a conflict", code, b2)
	}
	second, _ := decodeGrant(t, b2)

	if second.Access != "admin" {
		t.Errorf("[UPSERT-UPGRADES] the re-grant reported access=%q, expected the new tier", second.Access)
	}
	// ⚠ THE ROW CHECK IS HERE BECAUSE CONTROL C4 FOUND THIS ASSERTION COULD NOT SEE IT. C4 forges a
	// fixed, plausible uuid into every create response; `first.ID == second.ID` is then TRIVIALLY
	// satisfied — two responses compared against each other agree perfectly about a constant. The
	// first guard catches C4 via [LIST-AGREES], but this one is a separate test and was blind on its
	// own. Anchoring both ids to a row in Postgres is what makes the equality a statement about the
	// upsert rather than about the handler being self-consistent.
	if first.ID == "" || second.ID == "" {
		t.Errorf("[UPSERT-SAME-ID] a re-grant response carried an empty id (first=%q second=%q)",
			first.ID, second.ID)
	} else if first.ID != second.ID {
		t.Errorf("[UPSERT-SAME-ID] the re-grant reported a DIFFERENT id (%q then %q) — the row is "+
			"updated in place, so a second id means the response is not the row", first.ID, second.ID)
	} else if !f.rowExists(t, second.ID) {
		t.Errorf("[UPSERT-SAME-ID] both re-grant responses reported id=%q and NO SUCH ROW EXISTS — "+
			"the two bodies agree with each other and with nothing on disk", second.ID)
	}

	// ⚠ THE ROW COUNT IS ASSERTED AGAINST POSTGRES, NOT AGAINST THE ROUTE. "one row" is the half of
	// the upsert contract the response body cannot show.
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM permissions WHERE resource_type='space' AND resource_id=$1 AND subject_id=$2`,
		f.space, f.carol).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if n != 1 {
		t.Errorf("[UPSERT-ONE-ROW] %d rows for one (resource, subject) after two grants — the "+
			"ON CONFLICT collapse is gone", n)
	}

	// created_at must NOT move on an upgrade: the list route orders by it, and a re-grant that
	// re-stamps the time silently reorders the UI's "original owner first".
	if !second.CreatedAt.IsZero() && !first.CreatedAt.IsZero() &&
		second.CreatedAt.Sub(first.CreatedAt) > time.Millisecond {
		t.Errorf("[UPSERT-KEEPS-CREATED-AT] created_at moved on a re-grant (%v → %v)",
			first.CreatedAt, second.CreatedAt)
	}
}
