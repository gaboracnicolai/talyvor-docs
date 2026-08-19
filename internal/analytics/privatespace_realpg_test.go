package analytics_test

// THE FIFTH COPY OF THE PRIVATE-SPACE SEAM, AND THE ONE THE SWEEP THAT FOUND THE OTHER FOUR
// COULD NOT SEE.
//
// #78 (page.Handler /pages/search + /pages/stale), #79 (internal/ai /ask), #80 (freshness) and
// #81 (six MCP tools) each authorized the WORKSPACE and stopped. That sweep was indexed on the
// CALLERS OF THREE LEAKING STORE METHODS — `page.Store.Search`, `page.Store.GetStalePages`,
// `page.Store.SearchWithRank` (freshness/privatespace_realpg_test.go states the index in its own
// header). `analytics.Store.GetWorkspaceStats` calls NONE of them: it reads `page_views JOIN
// pages` itself, inside this package. An index built from three method names cannot report a
// fourth method, so this copy was invisible BY CONSTRUCTION rather than overlooked.
//
//	GET /v1/workspaces/{wsID}/analytics/pages   (handler.go: AuthorizeWorkspace, then stop)
//	    Store.GetWorkspaceStats(ctx, wsID, days)
//	        SELECT pv.page_id, MAX(p.title), COUNT(*) … WHERE pv.workspace_id = $1 … LIMIT 10
//	        → WorkspaceReadStats{MostReadPages, LeastReadPages, NeverRead, TotalViews, …}
//
// ⚠⚠ MEASURED ON REAL POSTGRES BEFORE THE FIX, and the shape of the evidence is what makes it a
// defect rather than a smell — THE SAME CALLER IS REFUSED THE SAME PAGE ONE ROUTE OVER:
//
//	GET /v1/spaces/{privSpace}/pages/{privPage}/analytics  as bob →  403 {"error":"forbidden"}
//	GET /v1/workspaces/{ws}/analytics/pages                as bob →  200, and the body carried
//	    {"page_id":"…","title":"Q3 Layoff Plan","total_views":7} in BOTH most_read_pages and
//	    least_read_pages, plus "never_read_count":1 counting a second private page.
//
// It is strictly MORE disclosive than the route that refuses: the per-page 200 for a page bob CAN
// read returns `"title":""` (that route does not populate it), while the rollup names the title.
// `/spaces/{space_id}/pages/{page_id}` is a working deep link and page_id is in the payload.
//
// ⚠ AND alice's RESPONSE WAS BYTE-IDENTICAL TO bob's — the endpoint had no per-caller component
// at all, so "does the caller matter here" had exactly one answer and it was no.
//
// ⚠⚠ THE FILTER CANNOT SIT AFTER THE `LIMIT 10`, AND THAT IS THE ONE PLACE THIS FIX DIFFERS FROM
// #80's. freshness had no LIMIT anywhere on its path, so filtering the returned list was complete.
// Here the ranking was truncated to 10 IN SQL: filtering afterwards would return 10-minus-hidden
// rows and silently shorten a reader's dashboard — a workspace whose ten most-read pages are all
// private would render an EMPTY "Most read" beside a non-zero total. The ranked window is now
// fetched WHOLE and truncated to 10 AFTER the visibility filter. [SHORT-LIST] is the case that
// holds it: it seeds 11 private + 2 visible pages, so a filter-after-LIMIT fix passes every
// absence assertion in this file and still fails there.
//
// THE CASES ARE ONE TEST ON PURPOSE, so a "fix" that just empties the rollup fails here rather
// than passing quietly:
//
//	[LEAK-LIST]  bob, no grant       the private page MUST NOT be in either ranked list ← the defect
//	[LEAK-COUNT] bob, no grant       never_read_count MUST NOT count a private page      ← the defect
//	[LEAK-TOTAL] bob, no grant       total_views MUST NOT include private-page views     ← the defect
//	[VISIBLE]    bob, no grant       the PUBLIC page MUST still appear, with its title   ← positive control
//	[OWNER]      alice, space creator the private page MUST appear                       ← owner-is-admin
//	[GRANTED]    bob + view grant    the private page MUST appear                        ← not `private = false`
//	[SHORT-LIST] bob, no grant       the ranked list MUST still be filled to its cap     ← filter-after-LIMIT
//	[HONEST-403] bob, no grant       the per-page route still refuses that page          ← the premise

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

func seedSpaceA(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator, private,
	).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageA(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, "body of "+title, `{"type":"doc"}`,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// seedViewsA writes n views that the 30-day window will actually select. A fixture whose rows
// fall outside the window produces an empty rollup, and an empty rollup satisfies every absence
// assertion in this file perfectly — [VISIBLE] is what refuses to let that count as a pass.
func seedViewsA(t *testing.T, d *testutil.DB, wsID, pageID, viewer string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := d.Pool.Exec(context.Background(),
			`INSERT INTO page_views (page_id, workspace_id, viewer_id, viewer_name, duration_sec, created_at)
			 VALUES ($1, $2, $3, $4, 10, NOW())`, pageID, wsID, viewer, viewer,
		); err != nil {
			t.Fatalf("seed view: %v", err)
		}
	}
}

type analyticsFixture struct {
	ws      string
	alice   string
	bob     string
	handler *analytics.Handler
	enf     *permission.Enforcer
}

func newAnalyticsFixture(t *testing.T, d *testutil.DB) *analyticsFixture {
	t.Helper()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	pageStore := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// cmd/docs/main.go's pageLooker, re-derived here rather than imported: it is the metadata the
	// permission engine decides on, and borrowing another test's helper borrows its evidence.
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
			WorkspaceID: pg.WorkspaceID,
			SpaceID:     pg.SpaceID, SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private,
			PageCreatedBy: pg.CreatedBy,
		}, nil
	}

	store := analytics.NewStore(d.Pool).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
	enf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))

	return &analyticsFixture{
		ws: ws, alice: alice, bob: bob,
		handler: analytics.NewHandler(store).WithAccess(enf),
		enf:     enf,
	}
}

// get drives the REAL handler as main.go mounts it, through chi, with the caller's memberships on
// the context exactly as authz.Middleware leaves them.
func (f *analyticsFixture) get(t *testing.T, email, memberID, path string) (int, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), email,
				[]authz.Membership{{WorkspaceID: f.ws, MemberID: memberID}})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Route("/v1", func(r chi.Router) { f.handler.Mount(r) })
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func (f *analyticsFixture) rollup(t *testing.T, email, memberID string) analytics.WorkspaceReadStats {
	t.Helper()
	code, body := f.get(t, email, memberID, "/v1/workspaces/"+f.ws+"/analytics/pages")
	if code != http.StatusOK {
		t.Fatalf("GET rollup as %s = %d, want 200: %s", email, code, body)
	}
	var out analytics.WorkspaceReadStats
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode rollup as %s: %v (%s)", email, err, body)
	}
	return out
}

func rowsHave(rows []analytics.ReadStats, pageID string) bool {
	for _, r := range rows {
		if r.PageID == pageID {
			return true
		}
	}
	return false
}

func titlesOf(rows []analytics.ReadStats) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

func TestWorkspaceAnalytics_PrivateSpace_NotRolledUpWithoutGrant_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)

	pubSpace := seedSpaceA(t, d, f.ws, f.alice, "Public Handbook", false)
	const pubTitle = "Onboarding Handbook"
	pubPage := seedPageA(t, d, f.ws, pubSpace, f.alice, pubTitle)
	seedViewsA(t, d, f.ws, pubPage, f.alice, 3)

	privSpace := seedSpaceA(t, d, f.ws, f.alice, "Board Private", true)
	const privTitle = "Q3 Layoff Plan"
	privPage := seedPageA(t, d, f.ws, privSpace, f.alice, privTitle)
	seedViewsA(t, d, f.ws, privPage, f.alice, 7)

	// A private page with NO views at all: it lands in the never_read cohort, which is a COUNT
	// rather than a row, so no absence-of-a-row assertion can see it.
	_ = seedPageA(t, d, f.ws, privSpace, f.alice, "Severance Model")

	// ── [HONEST-403] THE PREMISE. If bob could read this page anyway the rest of the file is
	// asserting nothing, so it is measured here rather than assumed.
	code, body := f.get(t, "bob@example.com", f.bob,
		"/v1/spaces/"+privSpace+"/pages/"+privPage+"/analytics")
	if code != http.StatusForbidden {
		t.Fatalf("[HONEST-403] per-page analytics for the private page as bob = %d, want 403: %s", code, body)
	}

	// ── [LEAK-LIST] + [LEAK-COUNT] + [LEAK-TOTAL] + [VISIBLE]: bob, a member with no grant.
	bobStats := f.rollup(t, "bob@example.com", f.bob)

	if rowsHave(bobStats.MostReadPages, privPage) {
		t.Errorf("[LEAK-LIST] most_read_pages names a page bob cannot open: titles=%v", titlesOf(bobStats.MostReadPages))
	}
	if rowsHave(bobStats.LeastReadPages, privPage) {
		t.Errorf("[LEAK-LIST] least_read_pages names a page bob cannot open: titles=%v", titlesOf(bobStats.LeastReadPages))
	}
	// The never-read cohort holds exactly one page bob cannot open and none he can, so the only
	// honest count for him is 0.
	if bobStats.NeverRead != 0 {
		t.Errorf("[LEAK-COUNT] never_read_count = %d for bob, want 0 — the only never-read page is in a private space he has no grant on", bobStats.NeverRead)
	}
	// 3 public views + 7 private ones were seeded. bob may account for the 3 only.
	if bobStats.TotalViews != 3 {
		t.Errorf("[LEAK-TOTAL] total_views = %d for bob, want 3 — the other 7 are views of a page he cannot open", bobStats.TotalViews)
	}
	if !rowsHave(bobStats.MostReadPages, pubPage) {
		t.Errorf("[VISIBLE] most_read_pages dropped the PUBLIC page: titles=%v", titlesOf(bobStats.MostReadPages))
	}

	// ── [OWNER] alice created both spaces, so the private page is hers to see.
	aliceStats := f.rollup(t, "alice@example.com", f.alice)
	if !rowsHave(aliceStats.MostReadPages, privPage) {
		t.Errorf("[OWNER] the space creator lost her own private page: titles=%v", titlesOf(aliceStats.MostReadPages))
	}
	if aliceStats.TotalViews != 10 {
		t.Errorf("[OWNER] total_views = %d for alice, want 10", aliceStats.TotalViews)
	}

	// ── [GRANTED] an explicit VIEW grant on the private SPACE, which is not the same state as
	// making the space public — a fix that keyed on `private = false` would fail here.
	permStore := permission.NewStore(d.Pool)
	if _, err := permStore.Grant(context.Background(), permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: privSpace, SubjectType: "member",
		SubjectID: f.bob, Access: permission.AccessView, WorkspaceID: f.ws, GrantedBy: f.alice,
	}); err != nil {
		t.Fatalf("grant bob view on the private space: %v", err)
	}
	grantedStats := f.rollup(t, "bob@example.com", f.bob)
	if !rowsHave(grantedStats.MostReadPages, privPage) {
		t.Errorf("[GRANTED] bob holds VIEW on the private space and still cannot see the page: titles=%v", titlesOf(grantedStats.MostReadPages))
	}
}

// TestWorkspaceAnalytics_RankedListIsFilledAfterFiltering_RealPG is [SHORT-LIST]: the case that a
// filter applied AFTER the SQL `LIMIT 10` passes every assertion above and still fails.
//
// 11 private pages out-rank 2 visible ones. Truncate-then-filter hands bob at most 10 rows of
// which every one is private ⇒ an EMPTY "Most read" beside a non-zero total. Fetch-then-truncate
// gives him both visible pages.
func TestWorkspaceAnalytics_RankedListIsFilledAfterFiltering_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)

	pubSpace := seedSpaceA(t, d, f.ws, f.alice, "Public Handbook", false)
	privSpace := seedSpaceA(t, d, f.ws, f.alice, "Board Private", true)

	// 11 private pages, 100..110 views — every one out-ranks both public pages.
	for i := 0; i < 11; i++ {
		p := seedPageA(t, d, f.ws, privSpace, f.alice, fmt.Sprintf("Secret %02d", i))
		seedViewsA(t, d, f.ws, p, f.alice, 100+i)
	}
	pubA := seedPageA(t, d, f.ws, pubSpace, f.alice, "Public A")
	seedViewsA(t, d, f.ws, pubA, f.alice, 2)
	pubB := seedPageA(t, d, f.ws, pubSpace, f.alice, "Public B")
	seedViewsA(t, d, f.ws, pubB, f.alice, 1)

	stats := f.rollup(t, "bob@example.com", f.bob)

	if !rowsHave(stats.MostReadPages, pubA) || !rowsHave(stats.MostReadPages, pubB) {
		t.Errorf("[SHORT-LIST] most_read_pages lost a page bob CAN read because 11 pages he cannot read were ranked above it: titles=%v", titlesOf(stats.MostReadPages))
	}
	if len(stats.MostReadPages) != 2 {
		t.Errorf("[SHORT-LIST] most_read_pages has %d rows, want exactly the 2 bob can read: titles=%v", len(stats.MostReadPages), titlesOf(stats.MostReadPages))
	}
	if stats.TotalViews != 3 {
		t.Errorf("[SHORT-LIST] total_views = %d for bob, want 3", stats.TotalViews)
	}
}
