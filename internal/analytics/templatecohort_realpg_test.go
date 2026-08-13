package analytics_test

// ONE SCREEN, TWO DEFINITIONS OF "A PAGE" — AND THE HALF THAT IGNORED `is_template` WAS THE HALF
// THAT RANKS.
//
// `GetWorkspaceStats` fills the Analytics screen from two cohorts over the same table:
//
//	ranked      SELECT pv.page_id, MAX(p.title), COUNT(*) FROM page_views pv JOIN pages p …
//	            → most_read_pages, least_read_pages ("Needs attention (lowest read)"),
//	              and — because both figures are derived from the surviving rows —
//	              total_views ("Views (30d)") and unique_viewers ("Unique visitors")
//	never read  SELECT p.id FROM pages p LEFT JOIN page_views pv … WHERE p.is_template = false
//	            → never_read_count ("Never read", rendered with a warning tone)
//
// The never-read half states its rule in its own words — "Templates are excluded (they're
// boilerplate, not content)". The ranked half names no template predicate at all. So a template
// that HAS been read is content, and a template that has NOT been read does not exist.
//
// ⚠⚠ MEASURED ON REAL POSTGRES AT `cd22162` THROUGH THE REAL ROUTE, BEFORE A LINE CHANGED. One
// workspace, one public space, four pages — a plain page with 3 views, a TEMPLATE with 5, a plain
// page with none, a template with none:
//
//	most_read[0]  = {"title":"TemplateRead","total_views":5}   ← the template, ranked FIRST
//	least_read[1] = {"title":"TemplateRead","total_views":5}   ← and in "Needs attention"
//	total_views   = 8   (5 of the 8 are the template's)
//	unique_viewers= 2   (one of the two read nothing but the template)
//	never_read    = 1   (the unread template is not among them)
//
// ⚠⚠ AND THE LIVE PATH IS THE FLIP, NOT THE FIXTURE — `is_template` IS IN `Update`'S ALLOWLIST.
// Measured on the same tree: a 9-view document `PATCH`ed to `is_template = true` leaves
// `SearchWithRank` (1 hit → 0) and leaves `WorkspacePageIDs`, the AI-cost syncer's enumerator
// (2 ids → 1) — and does NOT leave the ranking: still most_read[0], still in "Needs attention",
// its 9 views still in "Views (30d)". Marking a page as boilerplate removes it from every reader
// in this repository EXCEPT the dashboard that ranks it.
//
// ⚠ THE CENSUS THAT SAYS WHICH HALF IS THE OUTLIER, so this is not one comment's word against
// another's. Every non-test reader of `pages` that mentions templates EXCLUDES them:
// `page.SearchWithRank` (store.go), `search/semantic.go` ×2, `page.WorkspacePageIDs`,
// `analytics` never-read; `customdomain.Handler` filters `p.IsTemplate` in Go. Six sites, one
// direction. The ranked cohort is the only reader that ignores the column, and it ignores it by
// OMISSION — no comment anywhere defends counting templates.
//
// ⚠⚠ WHY THE FOUR FIGURES MOVE TOGETHER FROM ONE PREDICATE, AND WHY THAT IS THIS FILE'S OWN RULE
// RATHER THAN MY CHOICE: `total_views` is summed from the SURVIVING ranked rows and
// `unique_viewers` is queried over their ids, because "an unfiltered total beside a filtered list
// is itself a disclosure" (store.go, on the private-space filter). Narrowing the ranked fetch
// therefore narrows the totals by the rule already written above them.
//
// ⚠ THE ALTERNATIVE READING, STATED SO A REVERT IS ONE LINE AND ITS REASONING IS NOT LOST: one
// can hold that "Views (30d)" is a TRAFFIC figure and traffic on a template is still traffic. That
// reading survives only if the never-read count is widened to match — what cannot stand is the
// screen holding both definitions at once. This guard pins the direction the repository already
// committed to in writing; if the product wants the other one, [NEVER-READ-EXCLUDES-TEMPLATES]
// is the case that must change with it.
//
// THE CASES ARE ONE TEST ON PURPOSE, so a "fix" that empties the rollup fails here rather than
// passing quietly:
//
//	[RANKED-TEMPLATE] a viewed template is in NEITHER ranked list            ← the defect
//	[TOTAL-TEMPLATE]  total_views excludes its views                         ← the defect
//	[VIEWERS-TEMPLATE] a viewer who read only templates is not counted       ← the defect
//	[FLIP]            a read page marked as a template leaves the ranking    ← the live path
//	[SHORT-LIST]      the cap is still FILLED from content when templates
//	                  crowd the top of the ranking                           ← filter-after-cap
//	[CONTENT-VISIBLE] a plain viewed page still ranks, with title and count  ← positive control
//	[NEVER-READ-EXCLUDES-TEMPLATES] the other cohort is unchanged            ← must-stay-green

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// seedPageT is seedPageA plus the one column this file is about. Kept separate rather than
// widening seedPageA, whose callers are all about the private-space seam.
func seedPageT(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string, isTemplate bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content, is_template)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		spaceID, wsID, title, "pgt-"+title+"-"+wsID, author, "body of "+title, `{"type":"doc"}`, isTemplate,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q (template=%v): %v", title, isTemplate, err)
	}
	return id
}

func TestWorkspaceStats_TemplatesAreNotContent_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	sp := seedSpaceA(t, d, f.ws, f.alice, "tmplcohort", false)

	content := seedPageT(t, d, f.ws, sp, f.alice, "Content", false)
	tmplRead := seedPageT(t, d, f.ws, sp, f.alice, "TemplateRead", true)
	seedPageT(t, d, f.ws, sp, f.alice, "PlainUnread", false)
	seedPageT(t, d, f.ws, sp, f.alice, "TemplateUnread", true)

	// The template out-reads the content page, so it takes the top of the ranking — the
	// arrangement that makes [RANKED-TEMPLATE] fail loudly rather than by luck of ordering.
	seedViewsA(t, d, f.ws, content, "reader-of-content", 3)
	seedViewsA(t, d, f.ws, tmplRead, "reader-of-templates-only", 5)

	got := f.rollup(t, "alice@example.com", f.alice)

	if rowsHave(got.MostReadPages, tmplRead) || rowsHave(got.LeastReadPages, tmplRead) {
		t.Errorf("[RANKED-TEMPLATE] a TEMPLATE appears in the ranked lists: most_read=%v least_read=%v — "+
			"the same screen's never_read_count calls templates boilerplate and drops them",
			rowsHave(got.MostReadPages, tmplRead), rowsHave(got.LeastReadPages, tmplRead))
	}
	if got.TotalViews != 3 {
		t.Errorf("[TOTAL-TEMPLATE] total_views = %d, want 3 (the content page's) — the template's 5 "+
			"views are in the workspace's \"Views (30d)\" figure", got.TotalViews)
	}
	if got.UniqueViewers != 1 {
		t.Errorf("[VIEWERS-TEMPLATE] unique_viewers = %d, want 1 — a viewer who opened nothing but a "+
			"template is counted as a reader of this workspace's content", got.UniqueViewers)
	}

	// Positive control. Without it every assertion above is satisfied by an empty rollup, which is
	// the loudest possible pass: a screen that reports nothing agrees with every absence claim.
	if !rowsHave(got.MostReadPages, content) {
		t.Errorf("[CONTENT-VISIBLE] the plain viewed page is MISSING from most_read — the rollup was "+
			"emptied rather than narrowed (most_read=%d rows, least_read=%d rows)",
			len(got.MostReadPages), len(got.LeastReadPages))
	}
	for _, r := range got.MostReadPages {
		if r.PageID == content && (r.Title != "Content" || r.TotalViews != 3) {
			t.Errorf("[CONTENT-VISIBLE] the surviving row lost its payload: title=%q views=%d, want \"Content\"/3",
				r.Title, r.TotalViews)
		}
	}

	// Must stay green: the cohort that was already right. Two unread pages exist and exactly one
	// of them — the non-template — is counted. This is a FLOOR, not a defect probe: it passes on
	// the unmodified tree, and it is here so a fix that reaches for the never-read query instead
	// (dropping ITS predicate to "make the two agree") fails rather than passes.
	if got.NeverRead != 1 {
		t.Errorf("[NEVER-READ-EXCLUDES-TEMPLATES] never_read_count = %d, want 1 (PlainUnread only, "+
			"not TemplateUnread)", got.NeverRead)
	}
}

// [FLIP] is the live path: `is_template` is in page.Update's allowlist, so a document that has
// been read for months can become boilerplate with one PATCH. Measured before the fix: the page
// left SearchWithRank and WorkspacePageIDs and stayed at the top of the ranking.
func TestWorkspaceStats_FlipToTemplateLeavesTheRanking_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	sp := seedSpaceA(t, d, f.ws, f.alice, "tmplflip", false)
	ctx := authz.WithMemberships(context.Background(), "alice@example.com",
		[]authz.Membership{{WorkspaceID: f.ws, MemberID: f.alice}})

	doc := seedPageT(t, d, f.ws, sp, f.alice, "quarterly", false)
	keep := seedPageT(t, d, f.ws, sp, f.alice, "Keeper", false)
	seedViewsA(t, d, f.ws, doc, "v1", 9)
	seedViewsA(t, d, f.ws, keep, "v2", 2)

	ps := page.NewStore(d.Pool)
	if !rowsHave(f.rollup(t, "alice@example.com", f.alice).MostReadPages, doc) {
		t.Fatalf("[FLIP] premise: the document must rank BEFORE the flip, else this proves nothing")
	}
	if _, err := ps.UpdateInWorkspaces(ctx, doc, map[string]any{"is_template": true}, authz.WorkspaceIDs(ctx)); err != nil {
		t.Fatalf("[FLIP] mark as template: %v", err)
	}

	after := f.rollup(t, "alice@example.com", f.alice)
	if rowsHave(after.MostReadPages, doc) || rowsHave(after.LeastReadPages, doc) {
		t.Errorf("[FLIP] the page was marked as a template and still ranks: most_read=%v least_read=%v — "+
			"the same PATCH removed it from SearchWithRank and from WorkspacePageIDs",
			rowsHave(after.MostReadPages, doc), rowsHave(after.LeastReadPages, doc))
	}
	if after.TotalViews != 2 {
		t.Errorf("[FLIP] total_views = %d, want 2 — the flipped page's 9 views are still in \"Views (30d)\"",
			after.TotalViews)
	}
	// The other page must survive the flip, or "it left the ranking" is indistinguishable from
	// "the ranking is empty".
	if !rowsHave(after.MostReadPages, keep) {
		t.Errorf("[FLIP] the untouched page vanished too — the rollup emptied rather than narrowed")
	}
}

// [SHORT-LIST] is the case a filter applied AFTER the cap passes every assertion above and still
// fails. It is the same trap #94 recorded for the private-space filter: the ranked window is
// fetched WHOLE and capped in Go, so a template predicate bolted on after `visible[:rollupCap]`
// would hand back 10-minus-templates rows and silently shorten the dashboard.
func TestWorkspaceStats_TemplatesDoNotEatTheCap_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	sp := seedSpaceA(t, d, f.ws, f.alice, "tmplcap", false)

	// 11 templates out-read every content page, so they occupy the whole cap in the unfiltered
	// ranking; 10 content pages sit below them.
	for i := 0; i < 11; i++ {
		id := seedPageT(t, d, f.ws, sp, f.alice, "T"+string(rune('a'+i)), true)
		seedViewsA(t, d, f.ws, id, "tv", 50)
	}
	for i := 0; i < 10; i++ {
		id := seedPageT(t, d, f.ws, sp, f.alice, "C"+string(rune('a'+i)), false)
		seedViewsA(t, d, f.ws, id, "cv", 1+i)
	}

	got := f.rollup(t, "alice@example.com", f.alice)
	if len(got.MostReadPages) != 10 {
		t.Errorf("[SHORT-LIST] most_read has %d rows, want 10 — ten content pages qualify, so a short "+
			"list means templates were dropped AFTER the cap was applied", len(got.MostReadPages))
	}
	for _, r := range got.MostReadPages {
		if len(r.Title) > 0 && r.Title[0] == 'T' {
			t.Errorf("[SHORT-LIST] a template (%q) still occupies a slot in the cap", r.Title)
		}
	}
	if got.TotalViews != 55 {
		t.Errorf("[SHORT-LIST] total_views = %d, want 55 (1+…+10, the content pages only)", got.TotalViews)
	}
}
