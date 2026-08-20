package analytics_test

// THE WORKSPACE ROLL-UP'S TENANCY IS TWO SQL PREDICATES AND NOTHING ELSE, AND UNTIL THIS FILE
// NO TEST IN THE REPOSITORY COULD SEE EITHER OF THEM GO.
//
// tab-5r8k's predicate census (`scripts/w31-tenancypredicate-census-5r8k.py`, #186) rewrites one
// workspace conjunct at a time to `(workspace_id = $1 OR TRUE)` and runs the whole Go suite on a
// from-zero real Postgres. `analytics/store.go:454` — `GetWorkspaceStats`' ranked query — scored
// SILENT: 0 packages, 0 tests, exit 0. So did `:535`, its never-read cohort. The census recorded
// the number and said the per-site read was still owed, because a SILENT predicate is not
// automatically a defect: some are defence-in-depth behind another scoped filter (this file's
// own `:519`, the unique-viewers query, is exactly that — it is already bounded by
// `page_id = ANY(<already-filtered ids>)`).
//
// ⚠⚠ THESE TWO ARE NOT DEFENCE-IN-DEPTH. THE GO FILTER BELOW THEM CANNOT NARROW TO A WORKSPACE,
// AND SAYS SO ITSELF. `GetWorkspaceStats` drops every row the caller may not view by asking
// `spaceauth.Authorizer.AuthorizePageRead`, whose own doc comment reads: "resolved against every
// workspace the caller belongs to. It cannot express which ONE of those workspaces the page is
// in, so a caller who is a member of two workspaces satisfies it for a page in either — which is
// correct for reading and wrong for anything that must also name a single tenancy."
//
// A roll-up served at GET /v1/workspaces/{wsID}/analytics/pages is precisely a thing that must
// name a single tenancy. So for a caller with two memberships, the ONLY thing keeping workspace
// B's page titles, page ids and view counts off workspace A's Analytics screen is
// `WHERE pv.workspace_id = $1`, plus `WHERE p.workspace_id = $1` for the never-read count.
//
// ⚠ WHY IT WAS UNOBSERVABLE BY CONSTRUCTION RATHER THAN OVERLOOKED. Every existing case in this
// package drives the route through `analyticsFixture.get`, which builds the caller's context as
//
//	authz.WithMemberships(ctx, email, []authz.Membership{{WorkspaceID: f.ws, MemberID: memberID}})
//
// — ONE membership, always, hard-coded. The disagreeing case has therefore never been executed:
// `privatespace_realpg_test.go` proves a member of THIS workspace cannot see a private space in
// it, and no test asks what a member of ANOTHER workspace sees. A fixture that cannot express
// the second workspace cannot fail on it, which is why the census found 0 tests rather than a
// weak one. This file builds its own two-workspace caller for that reason.
//
// THE CASES ARE ONE TEST ON PURPOSE, so that a "fix" which merely empties the roll-up fails here
// instead of passing quietly:
//
//	[PREMISE-FOREIGN-READABLE] carol may read wsB's page at the per-page route  ← the load-bearing premise
//	[OWN-PRESENT]              wsA's own page IS in the roll-up, with its title ← refuses an empty pass
//	[FOREIGN-ABSENT]           wsB's page is in NEITHER ranked cohort            ← :454
//	[FOREIGN-TITLE-ABSENT]     wsB's page TITLE is in no row of either cohort    ← :454, MAX(p.title)
//	[FOREIGN-UNTOTALLED]       total_views counts wsA's views only               ← :454, summed downstream
//	[FOREIGN-UNCOUNTED]        never_read_count counts wsA's unread page only    ← :535
//	[FOREIGN-UNVIEWERED]       unique_visitors counts wsA's viewers only         ← :519 via visibleIDs
//
// [PREMISE-FOREIGN-READABLE] is the assertion that stops this file from passing vacuously. If
// carol could NOT read wsB's page, the Go gate would drop it whatever the SQL did, every absence
// assertion below would hold on a neutered predicate, and the guard would be green for a reason
// that has nothing to do with tenancy. It is measured through the REAL per-page route, not
// assumed — the same shape privatespace_realpg_test.go's [HONEST-403] uses for the opposite
// answer.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// crossWSFixture is the two-workspace twin of analyticsFixture. The store, the gate and the
// enforcer are wired exactly as newAnalyticsFixture wires them — the ONLY difference is that the
// caller's context carries BOTH memberships, which is the case the single-workspace fixture
// cannot express.
type crossWSFixture struct {
	wsA, wsB       string
	carolA, carolB string // one member row per workspace, as workspace_members is keyed
	handler        *analytics.Handler
	enf            *permission.Enforcer
}

func newCrossWSFixture(t *testing.T, d *testutil.DB) *crossWSFixture {
	t.Helper()
	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	const carol = "carol@example.com"

	pageStore := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// cmd/docs/main.go's pageLooker, re-derived rather than imported for the same reason
	// newAnalyticsFixture re-derives it: borrowing another test's helper borrows its evidence.
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

	return &crossWSFixture{
		wsA: wsA, wsB: wsB,
		carolA:  d.Member(t, wsA, carol),
		carolB:  d.Member(t, wsB, carol),
		handler: analytics.NewHandler(store).WithAccess(enf),
		enf:     enf,
	}
}

// get drives the real handler through chi with BOTH of carol's memberships on the context,
// exactly as authz.Middleware would leave them for a member of two workspaces.
func (f *crossWSFixture) get(t *testing.T, path string) (int, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), "carol@example.com", []authz.Membership{
				{WorkspaceID: f.wsA, MemberID: f.carolA},
				{WorkspaceID: f.wsB, MemberID: f.carolB},
			})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Route("/v1", func(r chi.Router) { f.handler.Mount(r) })
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

func TestWorkspaceStats_ForeignWorkspaceIsNotRolledUp_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newCrossWSFixture(t, d)

	// wsA — the workspace being asked about. One read page (3 views by ONE viewer) and one page
	// with no views at all, so every figure on the screen has a known wsA-only value.
	const ownTitle = "A Onboarding Handbook"
	spA := seedSpaceA(t, d, f.wsA, f.carolA, "Team A Handbook", false)
	pgA := seedPageA(t, d, f.wsA, spA, f.carolA, ownTitle)
	seedViewsA(t, d, f.wsA, pgA, "viewer-a", 3)
	_ = seedPageA(t, d, f.wsA, spA, f.carolA, "A Draft Never Opened")

	// wsB — the OTHER workspace carol belongs to. Deliberately NOT private: a private space
	// would be dropped by the page gate and this file would prove nothing about the SQL. Its
	// numbers are distinct from wsA's so a leak moves every figure rather than one.
	const foreignTitle = "B Acquisition Roadmap"
	spB := seedSpaceA(t, d, f.wsB, f.carolB, "Team B Strategy", false)
	pgB := seedPageA(t, d, f.wsB, spB, f.carolB, foreignTitle)
	seedViewsA(t, d, f.wsB, pgB, "viewer-b1", 5)
	seedViewsA(t, d, f.wsB, pgB, "viewer-b2", 4)
	_ = seedPageA(t, d, f.wsB, spB, f.carolB, "B Draft Never Opened")

	// ── [PREMISE-FOREIGN-READABLE] THE LOAD-BEARING PREMISE, MEASURED THROUGH THE REAL ROUTE.
	// Every absence assertion below is only about the SQL predicate if the Go gate would let
	// this page through. That gate is `AuthorizePageRead`, and the per-page analytics route is
	// the same question asked one route over: a 200 here means the gate says YES to wsB's page
	// for this caller, so the roll-up's tenancy rests on the WHERE clause alone.
	if code, body := f.get(t, "/v1/spaces/"+spB+"/pages/"+pgB+"/analytics"); code != http.StatusOK {
		t.Fatalf("[PREMISE-FOREIGN-READABLE] per-page analytics for wsB's page as carol = %d, want 200 "+
			"(if this is 403/404 the page gate — not the workspace predicate — is what keeps the row "+
			"out of the roll-up below, and every absence assertion in this test is vacuous): %s",
			code, body)
	}

	code, body := f.get(t, "/v1/workspaces/"+f.wsA+"/analytics/pages")
	if code != http.StatusOK {
		t.Fatalf("GET wsA roll-up as carol = %d, want 200: %s", code, body)
	}
	var got analytics.WorkspaceReadStats
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode roll-up: %v (%s)", err, body)
	}

	// ── [OWN-PRESENT] The positive control. An empty roll-up satisfies every absence assertion
	// in this test perfectly, so the wsA page must be PRESENT, in the cohort, with its title.
	if !rowsHave(got.MostReadPages, pgA) {
		t.Errorf("[OWN-PRESENT] wsA's own page %s is absent from most_read_pages %v — this test's "+
			"absence assertions cannot mean anything against an empty roll-up", pgA, titlesOf(got.MostReadPages))
	}
	if !containsTitle(titlesOf(got.MostReadPages), ownTitle) {
		t.Errorf("[OWN-PRESENT] wsA's own title %q is absent from most_read_pages %v",
			ownTitle, titlesOf(got.MostReadPages))
	}

	// ── [FOREIGN-ABSENT] The defect the predicate at store.go:454 prevents: wsB's page id in
	// wsA's ranked cohorts. page_id is a working /spaces/{space_id}/pages/{page_id} deep link.
	if rowsHave(got.MostReadPages, pgB) {
		t.Errorf("[FOREIGN-ABSENT] wsB's page %s appears in wsA's most_read_pages — a caller asked "+
			"about workspace %s was served a page from workspace %s", pgB, f.wsA, f.wsB)
	}
	if rowsHave(got.LeastReadPages, pgB) {
		t.Errorf("[FOREIGN-ABSENT] wsB's page %s appears in wsA's least_read_pages — a caller asked "+
			"about workspace %s was served a page from workspace %s", pgB, f.wsA, f.wsB)
	}

	// ── [FOREIGN-TITLE-ABSENT] Asserted separately from the id because the ranked query selects
	// `MAX(p.title)`: the title is the disclosive half, and it is what the private-space seam
	// measured as strictly more disclosive than the route that refuses.
	for _, cohort := range [][]analytics.ReadStats{got.MostReadPages, got.LeastReadPages} {
		if containsTitle(titlesOf(cohort), foreignTitle) {
			t.Errorf("[FOREIGN-TITLE-ABSENT] wsB's page title %q appears in wsA's roll-up %v",
				foreignTitle, titlesOf(cohort))
		}
	}

	// ── [FOREIGN-UNTOTALLED] total_views is SUMMED from the surviving ranked rows, so a leaked
	// row moves this figure too. 3 is wsA's own; 12 would be 3 + wsB's 9.
	if got.TotalViews != 3 {
		t.Errorf("[FOREIGN-UNTOTALLED] total_views = %d, want 3 (wsA's own views only; 12 means "+
			"wsB's 9 views were summed into another workspace's screen)", got.TotalViews)
	}

	// ── [FOREIGN-UNCOUNTED] The never-read cohort is the OTHER predicate (store.go:535) and it
	// is a COUNT — the shape no absence-of-a-row assertion can see, which is why it is asserted
	// on its value here rather than left to the cohort checks above.
	if got.NeverRead != 1 {
		t.Errorf("[FOREIGN-UNCOUNTED] never_read_count = %d, want 1 (wsA's unread page only; 2 means "+
			"wsB's unread page was counted into another workspace's screen)", got.NeverRead)
	}

	// ── [FOREIGN-UNVIEWERED] MEASURED, AND IT IS THE ONE FIGURE THAT DOES NOT MOVE UNDER A
	// RANKED-COHORT LEAK. Control C1 (the :454 scope neutered) was predicted to break this and
	// did not: unique_visitors stayed at 1. The unique-viewers query at store.go:519 is bounded
	// by `page_id = ANY(visibleIDs)` — which under C1 DOES contain the foreign page — but it
	// carries its own `workspace_id = $1` as well, and the foreign view rows carry wsB, so they
	// are excluded a second time. :519 is therefore defence-in-depth in the healthy case and
	// load-bearing in the leaked one, which is why a C1 leak discloses titles and totals but not
	// readers. The assertion stays because the figure is still derived from this cohort; what
	// changed is the claim made about it. See scripts/w31-crossws-rollup-controls-4j7q.py.
	if got.UniqueViewers != 1 {
		t.Errorf("[FOREIGN-UNVIEWERED] unique_visitors = %d, want 1 (wsA's single viewer; 3 means "+
			"wsB's two viewers were counted into another workspace's screen)", got.UniqueViewers)
	}
}

func containsTitle(titles []string, want string) bool {
	for _, tl := range titles {
		if strings.TrimSpace(tl) == want {
			return true
		}
	}
	return false
}
