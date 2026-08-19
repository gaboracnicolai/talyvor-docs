package sharing_test

// THE PUBLIC SHARE VIEWER IS THE ONLY UNAUTHENTICATED ROUTE THIS PRODUCT SERVES, AND NOTHING HAD
// EVER EXECUTED IT.
//
// Measured, not assumed, before this file existed: `go test -coverprofile -coverpkg=./... ./...`
// on a from-zero real Postgres, all 36 packages green, reported
//
//	internal/sharing/handler.go:62   MountPublic  0.0%
//	internal/sharing/handler.go:72   writeErr     0.0%
//	internal/sharing/handler.go:143  Revoke       0.0%
//	internal/sharing/handler.go:161  Public       0.0%
//
// `Create` and `List` were covered; the stranger-facing half and the revoke were not. So the two
// routes whose failure is a content leak — "the link still opens after it was revoked" and "the
// no-auth lane is reachable / is not reachable" — were carried entirely by review.
//
// ⚠ THE ASSERTION THAT WOULD HAVE BEEN A GUARD THAT CANNOT FAIL. "An unauthenticated GET returns
// 200" passes just as happily if the gateway proof were removed from EVERY /v1 route — that is
// the same 200. It is earned only by TestPublicLane_ExemptionIsNarrow below, which pins that the
// ADMIN routes in the same router still answer 401 to the same header-less request. One of those
// two tests alone is decoration; the pair is the boundary.
//
// ⚠ RED-FIRST IS NOT CLAIMED AND HERE IS WHY. Every assertion below passes on the unmodified tree:
// this lane was measured end-to-end on real Postgres (plain link, expired, password missing /
// wrong / right, unknown token, revoked, view_count, page in a private space) and behaved
// correctly on all of them. There is no defect here to fail first. Each assertion is instead
// earned by a POSITIVE CONTROL that mutates the shipped source and must turn that specific
// assertion red — scripts/w31-public-share-controls-8e4c.py, 8/8 as predicted, each naming its
// catcher before the run.
//
// ⚠ ONE THING MEASURED HERE AND DELIBERATELY NOT ASSERTED, because it is a product decision and
// not this file's to make: a share link on a page in a PRIVATE space serves 200 to a stranger.
// The sibling public-publishing surface in this repo answers the opposite way — customdomain
// refuses to map a private space (store.go:193) AND re-checks it at serve time (store.go:256-262,
// the #115 fix), so the same question has two answers in one repository. Recorded in the queue.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/sharing"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const publicLaneSecret = "sec4-test-gateway-secret-0123456789"

// publicLaneChain builds the REAL chain cmd/docs/main.go builds: the /v1 group with
// gatewayauth + authz above it, both handed the SHARED exemption predicates rather than a
// re-declared literal. gatewayauth/exempt.go says why that matters — a test that re-declares
// the predicate is testing a router that may not exist.
func publicLaneChain(t *testing.T, d *testutil.DB) http.Handler {
	t.Helper()
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

	// The same projection cmd/docs/main.go wires into NewHandler. Kept field-for-field so
	// TestPublicLane_PayloadCarriesNoEditMetadata is a statement about the SHIPPED shape.
	loader := func(ctx context.Context, pageID string) (*sharing.PublicPage, error) {
		p, err := pageStore.GetByID(ctx, pageID)
		if err != nil || p == nil {
			return nil, err
		}
		return &sharing.PublicPage{
			ID:          p.ID,
			Title:       p.Title,
			Icon:        p.Icon,
			Content:     p.Content,
			ContentText: p.ContentText,
			UpdatedAt:   p.UpdatedAt.Format(time.RFC3339),
		}, nil
	}

	h := sharing.NewHandler(sharing.NewStore(d.Pool), loader).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(gatewayauth.Middleware(publicLaneSecret, gatewayauth.ExemptTransitProof))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), gatewayauth.ExemptMembership))
		h.Mount(r)
		h.MountPublic(r)
	})
	return r
}

type publicLaneFixture struct {
	srv    http.Handler
	store  *sharing.Store
	pageID string
	ws     string
	author string
	body   string
}

func newPublicLaneFixture(t *testing.T, d *testutil.DB) *publicLaneFixture {
	t.Helper()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Public Lane Doc")
	const body = "the body a stranger must only see through a live link"
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE pages SET content = $2, content_text = $3 WHERE id = $1`,
		pageID, `{"type":"doc"}`, body); err != nil {
		t.Fatalf("seed page content: %v", err)
	}
	return &publicLaneFixture{
		srv: publicLaneChain(t, d), store: sharing.NewStore(d.Pool),
		pageID: pageID, ws: ws, author: author, body: body,
	}
}

// link mints a share link on the fixture page. A t.Fatalf here is a SETUP failure, not a caught
// mutation — the control harness scores by the named failing test AND its tag, so a control that
// only breaks minting is visible as such rather than counted as a catch.
func (f *publicLaneFixture) link(t *testing.T, expires *time.Time, password string) *sharing.ShareLink {
	t.Helper()
	l, err := f.store.Create(context.Background(), f.pageID, f.ws, f.author, permission.AccessView, expires, password)
	if err != nil {
		t.Fatalf("[SETUP] mint share link: %v", err)
	}
	return l
}

// get drives the chain with NO headers whatsoever — no x-gateway-auth, no x-user-email. That
// absence is the point of every test in this file.
func (f *publicLaneFixture) get(t *testing.T, url string) (int, string) {
	t.Helper()
	code, body, _ := f.getFull(t, url)
	return code, body
}

func (f *publicLaneFixture) getFull(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec.Code, rec.Body.String(), rec.Header()
}

func (f *publicLaneFixture) open(t *testing.T, token, query string) (int, string) {
	t.Helper()
	return f.get(t, "/v1/public/s/"+token+query)
}

// TestPublicLane_StrangerCanOpenALiveLink is HALF a guard on its own — see ExemptionIsNarrow.
func TestPublicLane_StrangerCanOpenALiveLink_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	l := f.link(t, nil, "")

	code, body, hdr := f.getFull(t, "/v1/public/s/"+l.Token)
	if code != http.StatusOK {
		t.Errorf("[STRANGER-CAN-OPEN] want 200 for a live link with no headers, got %d: %s", code, body)
	}
	// ⚠ THIS ASSERTION EXISTS BECAUSE A CONTROL FOUND IT MISSING, NOT BECAUSE ANYONE READ FOR IT.
	// C9 of the harness blanked the header and NOTHING went red: the handler's own comment calls
	// X-Powered-By "the Phase 8 contract", and the only thing pinned was the powered_by field in
	// the body — a different string, set on a different line, that a header change does not touch.
	if got := hdr.Get("X-Powered-By"); got != "Talyvor Docs" {
		t.Errorf("[POWERED-BY-HEADER] want the Phase 8 header contract, got %q", got)
	}
	var out struct {
		Page struct {
			ContentText string `json:"content_text"`
		} `json:"page"`
		Access      string `json:"access"`
		HasPassword bool   `json:"has_password"`
		PoweredBy   string `json:"powered_by"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("[STRANGER-CAN-OPEN] response is not JSON: %v (%s)", err, body)
	}
	// The CONTENT, not merely a 200 — a 200 carrying an empty page is the failure this catches.
	if out.Page.ContentText != f.body {
		t.Errorf("[STRANGER-CAN-OPEN] want the page body served, got %q", out.Page.ContentText)
	}
	if out.Access != string(permission.AccessView) {
		t.Errorf("[STRANGER-CAN-OPEN] want access=view, got %q", out.Access)
	}
	if out.HasPassword {
		t.Errorf("[STRANGER-CAN-OPEN] a link minted with no password reports has_password=true")
	}
	if out.PoweredBy != "Talyvor Docs" {
		t.Errorf("[STRANGER-CAN-OPEN] want the Phase 8 powered_by contract, got %q", out.PoweredBy)
	}
}

// TestPublicLane_ExemptionIsNarrow is what makes the test above mean anything: the SAME
// header-less request to an ADMIN route in the SAME router must still be refused by the gateway.
// Without this, deleting the gateway middleware outright would leave every other test green.
func TestPublicLane_ExemptionIsNarrow_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)

	for _, tc := range []struct {
		name, method, url string
	}{
		{"list share links", http.MethodGet, "/v1/spaces/sp/pages/" + f.pageID + "/share"},
		{"mint a share link", http.MethodPost, "/v1/spaces/sp/pages/" + f.pageID + "/share"},
		{"revoke a share link", http.MethodDelete, "/v1/spaces/sp/pages/" + f.pageID + "/share/any"},
	} {
		rec := httptest.NewRecorder()
		f.srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.url, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("[EXEMPTION-IS-NARROW] %s with no gateway proof: want 401, got %d: %s",
				tc.name, rec.Code, rec.Body.String())
		}
	}
}

// TestPublicLane_ExpiredLinkIsGone pins that expiry is enforced on the READ, not only recorded.
func TestPublicLane_ExpiredLinkIsGone_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	past := time.Now().UTC().Add(-time.Hour)
	l := f.link(t, &past, "")

	code, body := f.open(t, l.Token, "")
	if code != http.StatusGone {
		t.Errorf("[EXPIRED-IS-GONE] want 410 for an expired link, got %d: %s", code, body)
	}
	assertNoLeak(t, "[EXPIRED-IS-GONE]", body, f.body)
}

// TestPublicLane_PasswordIsEnforced covers all three password states in one place: a link with a
// password must not serve its page until the RIGHT one arrives.
func TestPublicLane_PasswordIsEnforced_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	l := f.link(t, nil, "hunter2")

	code, body := f.open(t, l.Token, "")
	if code != http.StatusUnauthorized {
		t.Errorf("[PASSWORD-REQUIRED] want 401 with no password, got %d: %s", code, body)
	}
	assertNoLeak(t, "[PASSWORD-REQUIRED]", body, f.body)

	code, body = f.open(t, l.Token, "?password=not-the-password")
	if code != http.StatusUnauthorized {
		t.Errorf("[PASSWORD-WRONG] want 401 with the wrong password, got %d: %s", code, body)
	}
	assertNoLeak(t, "[PASSWORD-WRONG]", body, f.body)

	// The must-stay-green half: a guard that refuses everything is not a guard either.
	code, body = f.open(t, l.Token, "?password=hunter2")
	if code != http.StatusOK {
		t.Errorf("[PASSWORD-RIGHT] want 200 with the right password, got %d: %s", code, body)
	}
	if !containsBody(body, f.body) {
		t.Errorf("[PASSWORD-RIGHT] the right password did not serve the page: %s", body)
	}
}

// TestPublicLane_RevokeActuallyRevokes is the assertion the 0.0% on Revoke left uncovered: the
// route reporting {"ok":true} and the token no longer opening are two different facts.
func TestPublicLane_RevokeActuallyRevokes_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	l := f.link(t, nil, "")

	if code, _ := f.open(t, l.Token, ""); code != http.StatusOK {
		t.Fatalf("[SETUP] the link did not open before revoking: %d", code)
	}
	if err := f.store.Revoke(context.Background(), l.ID, f.pageID); err != nil {
		t.Fatalf("[SETUP] revoke reported an error: %v", err)
	}
	code, body := f.open(t, l.Token, "")
	if code != http.StatusNotFound {
		t.Errorf("[REVOKE-REVOKES] a revoked token still answers %d: %s", code, body)
	}
	assertNoLeak(t, "[REVOKE-REVOKES]", body, f.body)
}

// TestPublicLane_RevokeIsScopedToItsPage pins the ce8bfe3 cross-tenant fix from the OUTSIDE: an
// admin of page A revoking by id must not be able to kill page B's link. The store returns
// ErrShareLinkNotFound; the observable consequence is that B's token keeps working.
func TestPublicLane_RevokeIsScopedToItsPage_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	victimLink := f.link(t, nil, "")

	otherWS := d.Workspace(t)
	otherAuthor := d.Member(t, otherWS, "mallory@example.com")
	otherPage := d.Page(t, otherWS, otherAuthor, "Another Tenant Doc")

	err := f.store.Revoke(context.Background(), victimLink.ID, otherPage)
	if err == nil {
		t.Errorf("[REVOKE-SCOPED] revoking another page's link by id reported success")
	}
	code, _ := f.open(t, victimLink.Token, "")
	if code != http.StatusOK {
		t.Errorf("[REVOKE-SCOPED] a cross-page revoke killed the victim's link (now %d)", code)
	}
}

// TestPublicLane_UnknownTokenIsNotFound — no oracle, no leak, for a token that never existed.
func TestPublicLane_UnknownTokenIsNotFound_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)

	code, body := f.open(t, "deadbeefdeadbeefdeadbeefdeadbeef", "")
	if code != http.StatusNotFound {
		t.Errorf("[UNKNOWN-TOKEN] want 404, got %d: %s", code, body)
	}
	assertNoLeak(t, "[UNKNOWN-TOKEN]", body, f.body)
}

// publicPageKeys is the lean projection sharing.PublicPage promises: "we deliberately omit edit
// metadata (created_by, view_count, AI cost, etc) — public readers don't need it." Pinned as a
// LITERAL SET, so a field added to PublicPage fails here rather than shipping to strangers.
var publicPageKeys = []string{"content", "content_text", "icon", "id", "title", "updated_at"}

func TestPublicLane_PayloadCarriesNoEditMetadata_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	l := f.link(t, nil, "")

	code, body := f.open(t, l.Token, "")
	if code != http.StatusOK {
		t.Fatalf("[SETUP] live link did not open: %d %s", code, body)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("[LEAN-PROJECTION] response is not a JSON object: %v", err)
	}
	var pageObj map[string]json.RawMessage
	if err := json.Unmarshal(envelope["page"], &pageObj); err != nil {
		t.Fatalf("[LEAN-PROJECTION] `page` is not a JSON object: %v", err)
	}
	got := make([]string, 0, len(pageObj))
	for k := range pageObj {
		got = append(got, k)
	}
	sort.Strings(got)
	if len(got) != len(publicPageKeys) {
		t.Errorf("[LEAN-PROJECTION] public page keys = %v, want exactly %v", got, publicPageKeys)
		return
	}
	for i := range got {
		if got[i] != publicPageKeys[i] {
			t.Errorf("[LEAN-PROJECTION] public page keys = %v, want exactly %v", got, publicPageKeys)
			return
		}
	}
	// The envelope itself must not grow a hash either — stripHash is what keeps it out.
	if _, ok := envelope["password_hash"]; ok {
		t.Errorf("[LEAN-PROJECTION] the public envelope carries password_hash")
	}
}

// assertNoLeak is the half a status-code assertion misses: a refusal that still ships the body is
// not a refusal. Every non-200 path above is checked for the page text.
func assertNoLeak(t *testing.T, tag, body, secret string) {
	t.Helper()
	if containsBody(body, secret) {
		t.Errorf("%s the refusal response carried the page body: %s", tag, body)
	}
}

func containsBody(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
