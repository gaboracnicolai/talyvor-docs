package page_test

// THE SECOND "New page" CLICK IN A SPACE FAILED, AND IT FAILED WITH A RAW POSTGRES ERROR.
//
// `pages` carries `UNIQUE (space_id, slug)` (migrations/0002_pages.sql:36) and Store.Create derives
// the slug from the title with no uniqueness handling at all. Measured through the shipped REST
// route before the fix, with EXACTLY the body frontend/src/pages/SpaceView.tsx posts:
//
//	POST /v1/spaces/{id}/pages  {"title":"Untitled"}   -> 201
//	POST /v1/spaces/{id}/pages  {"title":"Untitled"}   -> 400
//	  {"error":"page: insert: ERROR: duplicate key value violates unique constraint
//	            \"pages_space_id_slug_key\" (SQLSTATE 23505)","code":"CREATE_FAILED"}
//	pages in the space: 1
//
// The SPA's "New page" button posts a HARDCODED {"title":"Untitled"} — it is not a title the user
// chose, it is the default the button ships. So the product's most ordinary sequence, clicking
// "New page" twice in one space, could not be performed at all until somebody renamed the first
// page, and what the user was shown was a constraint name.
//
// ⚠ IT IS NOT ONE DOOR. `INSERT INTO pages` exists in exactly ONE place in this repository (the
// statement in Store.Create) and SIX callers reach it — the block above that INSERT already names
// them, because the page-creation METRIC was fixed for the same reason. A census of all six found
// that NOT ONE sets Page.Slug, so in production the slug is ALWAYS derived and every door had the
// collision: [TEMPLATE-TWICE] below drives templatelib, where using the same built-in template
// twice in one space returned the same 400 (measured), and the importer lands a whole Confluence
// export into one space, where repeated titles are ordinary.
//
// ⚠ WHY NOTHING FAILED FOR IT. Every real-PG test in this package that creates two pages in one
// space gives them DIFFERENT titles — parentdepth_realpg_test.go:68 even carries the comment
// "slug uniqueness: pages carry a UNIQUE (space_id, slug)" beside a counter that exists to keep
// its own titles distinct. The constraint was known and worked around test by test; the shipped
// default title was the one input nobody wrote a case for.
//
// THE FIX IS IN THE STORE, NOT IN THE CALLERS, and it mirrors this file's existing
// retry-on-unique-violation (appendVersion / maxVersionAttempts / uniqueViolation): the store owns
// the slug, so it is the store that guarantees it is unique inside the space.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

func seedSpace(t *testing.T, d *testutil.DB, wsID, owner, slug string) string {
	t.Helper()
	sp, err := space.NewStore(d.Pool).Create(context.Background(), model.Space{
		WorkspaceID: wsID, Name: "Space " + slug, Slug: slug, CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space %s: %v", slug, err)
	}
	return sp.ID
}

func TestCreate_TwoPagesCanShareATitleInOneSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	chain := newV1Chain(t, d)
	ctx := context.Background()

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	spaceID := seedSpace(t, d, ws, owner, "sc-"+owner[len(owner)-8:])

	post := func(body string) (int, string) {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/spaces/"+spaceID+"/pages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Gateway-Auth", testGatewaySecret)
		r.Header.Set("X-User-Email", "owner@corp.com")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr.Code, rr.Body.String()
	}

	// ── THE DEFECT ── the SPA's button, clicked twice, over the real chain.
	if code, body := post(`{"title":"Untitled"}`); code != 201 {
		t.Fatalf("[SECOND-NEW-PAGE] the FIRST New page click failed: %d %s", code, body)
	}
	if code, body := post(`{"title":"Untitled"}`); code != 201 {
		t.Errorf("[SECOND-NEW-PAGE] the second \"New page\" click in the same space returned %d %s — "+
			"the SPA posts a hardcoded {\"title\":\"Untitled\"}, so this is the ordinary sequence, "+
			"not a user typing a duplicate title", code, body)
	}

	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pages WHERE space_id = $1`, spaceID).Scan(&n); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if n != 2 {
		t.Errorf("[SECOND-NEW-PAGE] %d pages in the space after two \"New page\" clicks, want 2 — "+
			"assert on the ROWS, not just the status: a 201 that landed nothing would pass a "+
			"status-only check", n)
	}

	// ── THE SLUGS ── disambiguated, and BOTH still addressable. A shared slug would satisfy the
	// count above while breaking GetBySlug, which is how the public custom-domain route and the MCP
	// get_page tool address a page.
	slugs := map[string]bool{}
	rows, err := d.Pool.Query(ctx, `SELECT slug FROM pages WHERE space_id = $1 ORDER BY created_at`, spaceID)
	if err != nil {
		t.Fatalf("read slugs: %v", err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan slug: %v", err)
		}
		slugs[s] = true
	}
	rows.Close()
	if len(slugs) != 2 {
		t.Errorf("[SLUGS-DIFFER] the two pages hold %d distinct slugs, want 2: %v", len(slugs), slugs)
	}

	// ── THE FIRST SLUG IS UNCHANGED ── the must-stay-green companion. Without it, a "fix" that
	// suffixes EVERY page would satisfy every assertion above while changing the address of every
	// page this product has ever created.
	if !slugs["untitled"] {
		t.Errorf("[FIRST-SLUG-UNCHANGED] no page holds the plain derived slug %q — the first page "+
			"with a title must keep slugify(title) verbatim; got %v", "untitled", slugs)
	}

	// ── THE TITLE IS THE USER'S ── disambiguate the slug, never the title. Renaming somebody's page
	// to make an INSERT succeed would also make every assertion above pass.
	var titles []string
	trows, err := d.Pool.Query(ctx, `SELECT title FROM pages WHERE space_id = $1`, spaceID)
	if err != nil {
		t.Fatalf("read titles: %v", err)
	}
	for trows.Next() {
		var s string
		if err := trows.Scan(&s); err != nil {
			t.Fatalf("scan title: %v", err)
		}
		titles = append(titles, s)
	}
	trows.Close()
	for _, got := range titles {
		if got != "Untitled" {
			t.Errorf("[TITLE-UNTOUCHED] a page's title is %q, want %q for both — the slug is what "+
				"gets disambiguated; the title is what the user typed", got, "Untitled")
		}
	}
}

// THE SUFFIX SEQUENCE RESTARTS IN EVERY SPACE.
//
// ⚠ THE FIRST VERSION OF THIS TEST COULD NOT FAIL, AND A CONTROL IS THE ONLY REASON THAT IS KNOWN.
// It created ONE "Runbook" in each of two spaces and asserted both kept the plain slug. C5 — the
// plausible wrong implementation, counting matching slugs across the WORKSPACE instead of the space
// — scored NOT CAUGHT: the retry only runs AFTER a (space_id, slug) violation, and a lone page in
// its own space never collides, so the mutated line never executed. The property was true by
// construction and the assertion was watching nothing.
//
// It now makes each space COLLIDE on its own, which is the only shape in which the two scopes give
// different answers: per-space, both spaces read runbook / runbook-2; counted over the workspace,
// the second space's pages land on runbook / runbook-4.
func TestCreate_SlugSuffixRestartsPerSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	a := seedSpace(t, d, ws, owner, "sa-"+owner[len(owner)-8:])
	b := seedSpace(t, d, ws, owner, "sb-"+owner[len(owner)-8:])
	ps := page.NewStore(d.Pool)

	for _, spaceID := range []string{a, b} {
		for i := 0; i < 2; i++ {
			if _, err := ps.Create(ctx, model.Page{
				SpaceID: spaceID, WorkspaceID: ws, Title: "Runbook", CreatedBy: owner,
			}); err != nil {
				t.Fatalf("create #%d in space: %v", i+1, err)
			}
		}
	}
	for _, spaceID := range []string{a, b} {
		var got []string
		rows, err := d.Pool.Query(ctx,
			`SELECT slug FROM pages WHERE space_id = $1 ORDER BY slug`, spaceID)
		if err != nil {
			t.Fatalf("read slugs: %v", err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, s)
		}
		rows.Close()
		want := []string{"runbook", "runbook-2"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("[SCOPED-TO-SPACE] a space holding two %q pages reads %v, want %v — "+
				"uniqueness is (space_id, slug), so the suffix is counted inside the space and "+
				"an unrelated space must not push it", "Runbook", got, want)
		}
	}
}

func TestCreate_ManySameTitledPagesInOneSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	spaceID := seedSpace(t, d, ws, owner, "sm-"+owner[len(owner)-8:])
	ps := page.NewStore(d.Pool)

	// Five is not arbitrary: it is maxVersionAttempts, the bound the sibling retry in this file
	// uses. A fix bounded at one retry passes every assertion above and fails here on the third.
	const want = 5
	for i := 0; i < want; i++ {
		if _, err := ps.Create(ctx, model.Page{
			SpaceID: spaceID, WorkspaceID: ws, Title: "Untitled", CreatedBy: owner,
		}); err != nil {
			t.Fatalf("[MANY] create #%d of %d failed: %v", i+1, want, err)
		}
	}
	var n int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pages WHERE space_id = $1`, spaceID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != want {
		t.Errorf("[MANY] %d pages after %d creates of the same title", n, want)
	}
	// Every slug distinct — the count alone would pass if two rows somehow shared one.
	var distinct int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(DISTINCT slug) FROM pages WHERE space_id = $1`, spaceID).Scan(&distinct); err != nil {
		t.Fatalf("count distinct: %v", err)
	}
	if distinct != want {
		t.Errorf("[MANY] %d distinct slugs across %d pages", distinct, want)
	}
}
