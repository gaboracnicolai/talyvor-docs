package page

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/model"
)

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return newStore(pool), pool
}

func ptrStr(s string) *string { return &s }
func ptrTime(t time.Time) *time.Time { return &t }

func pageCols() []string {
	return []string{
		"id", "space_id", "workspace_id", "parent_id", "title", "slug",
		"content", "content_text", "icon", "cover_url",
		"position", "depth", "is_template", "created_by", "updated_by",
		"linked_issues", "ai_cost_usd",
		"view_count", "last_viewed_at",
		"last_verified_at", "verified_by", "stale_after_days",
		"doc_status",
		"locked", "locked_by", "locked_at",
		"page_type",
		"created_at", "updated_at",
	}
}

func pageRow(id, title, slug string, depth int, parent *string) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows(pageCols()).AddRow(
		id, "sp-1", "ws-1", parent, title, slug,
		"{}", "", "", "",
		float64(0), depth, false, "creator", "creator",
		[]string{}, float64(0),
		0, (*time.Time)(nil),
		(*time.Time)(nil), (*string)(nil), 0,
		"draft",
		false, (*string)(nil), (*time.Time)(nil),
		"document",
		now, now,
	)
}

// ─── 1. Create — auto-slug, depth = parent.depth+1, writes version 1

func TestCreate_AutoSlugAndDepthAndVersion(t *testing.T) {
	store, pool := newMockStore(t)
	parent := "parent-page"
	// Parent depth lookup.
	pool.ExpectQuery(`SELECT depth FROM pages WHERE id`).
		WithArgs(parent).
		WillReturnRows(pgxmock.NewRows([]string{"depth"}).AddRow(int(1)))
	// INSERT — store derives slug "my-new-page" from title.
	pool.ExpectQuery(`INSERT INTO pages`).
		WithArgs("sp-1", "ws-1", &parent, "My New Page", "my-new-page",
			"{}", "", "", "", float64(0), 2, false, "creator", "creator",
			[]string{}, float64(0), 0).
		WillReturnRows(pageRow("pg-1", "My New Page", "my-new-page", 2, &parent))
	// Version 1 insert — now carries the page's workspace_id.
	pool.ExpectExec(`INSERT INTO page_versions \(page_id, workspace_id, version`).
		WithArgs("pg-1", "ws-1", 1, "My New Page", "{}", "creator").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	out, err := store.Create(context.Background(), model.Page{
		SpaceID:     "sp-1",
		WorkspaceID: "ws-1",
		ParentID:    &parent,
		Title:       "My New Page",
		CreatedBy:   "creator",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out.Slug != "my-new-page" {
		t.Errorf("slug = %q", out.Slug)
	}
	if out.Depth != 2 {
		t.Errorf("depth = %d, want 2", out.Depth)
	}
}

func TestCreate_RejectsDeepNesting(t *testing.T) {
	store, pool := newMockStore(t)
	parent := "deep"
	// Parent is already at depth 5 → would make a depth-6 child.
	pool.ExpectQuery(`SELECT depth FROM pages WHERE id`).
		WithArgs(parent).
		WillReturnRows(pgxmock.NewRows([]string{"depth"}).AddRow(int(5)))
	_, err := store.Create(context.Background(), model.Page{
		SpaceID: "sp-1", WorkspaceID: "ws-1",
		ParentID: &parent, Title: "Too deep", CreatedBy: "u",
	})
	if err == nil {
		t.Fatal("expected error for nesting beyond depth 5")
	}
}

// ─── 2. GetByID ────────────────────────────────────────────

func TestGetByID_Roundtrip(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`FROM pages WHERE id = \$1`).
		WithArgs("pg-1").
		WillReturnRows(pageRow("pg-1", "Hello", "hello", 0, nil))
	out, err := store.GetByID(context.Background(), "pg-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if out.Title != "Hello" {
		t.Errorf("title = %q", out.Title)
	}
}

// ─── 3. GetBySlug ──────────────────────────────────────────

func TestGetBySlug_Lookup(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`WHERE space_id = \$1 AND slug = \$2`).
		WithArgs("sp-1", "hello").
		WillReturnRows(pageRow("pg-1", "Hello", "hello", 0, nil))
	if _, err := store.GetBySlug(context.Background(), "sp-1", "hello"); err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
}

// ─── 4. List tree ──────────────────────────────────────────

func TestList_TreeOrderedByPosition(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`FROM pages WHERE space_id`).
		WithArgs("sp-1", 100, 0).
		WillReturnRows(pgxmock.NewRows(pageCols()).
			AddRow(rowVals("a", "Page A", "page-a", 0, nil)...).
			AddRow(rowVals("b", "Page B", "page-b", 1, ptrStr("a"))...))
	out, err := store.List(context.Background(), PageFilter{
		SpaceID: "sp-1",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("got %d, want 2", len(out))
	}
}

// rowVals returns the column values for a basic page row.
func rowVals(id, title, slug string, depth int, parent *string) []driverValue {
	now := time.Now().UTC()
	return []driverValue{
		id, "sp-1", "ws-1", parent, title, slug,
		"{}", "", "", "",
		float64(0), depth, false, "creator", "creator",
		[]string{}, float64(0),
		0, (*time.Time)(nil),
		(*time.Time)(nil), (*string)(nil), 0,
		"draft",
		false, (*string)(nil), (*time.Time)(nil),
		"document",
		now, now,
	}
}

type driverValue = any

// ─── 5. Update creates version (and updates content_text)

func TestUpdate_AppendsNewVersionOnContentChange(t *testing.T) {
	store, pool := newMockStore(t)
	// Re-fetch existing page so the store can compare content.
	pool.ExpectQuery(`SELECT .* FROM pages WHERE id = \$1`).
		WithArgs("pg-1").
		WillReturnRows(pageRow("pg-1", "Old", "old", 0, nil))
	// Updated page returned from UPDATE. The SET clause includes
	// content, content_text, updated_by, updated_at, plus the WHERE
	// id arg — five total.
	pool.ExpectQuery(`UPDATE pages SET`).
		WithArgs(
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(),
		).
		WillReturnRows(pageRow("pg-1", "Old", "old", 0, nil))
	// Lookup max version.
	pool.ExpectQuery(`SELECT COALESCE\(MAX\(version\), 0\) FROM page_versions`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows([]string{"max"}).AddRow(int(3)))
	// Insert version 4 — now carries workspace_id (from the updated page). History is
	// append-only: NO prune DELETE follows (removed; see TestVersions_AppendOnly_*_RealPG).
	pool.ExpectExec(`INSERT INTO page_versions \(page_id, workspace_id, version`).
		WithArgs("pg-1", "ws-1", 4, "Old", "{}", "editor").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if _, err := store.Update(context.Background(), "pg-1", map[string]any{
		"content":    `{"type":"doc"}`,
		"updated_by": "editor",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// ─── 6. Delete reparents children ──────────────────────────

func TestDelete_ReparentsChildren(t *testing.T) {
	store, pool := newMockStore(t)
	// Lookup the page's parent so children can inherit it.
	pool.ExpectQuery(`SELECT parent_id, depth FROM pages WHERE id`).
		WithArgs("pg-mid").
		WillReturnRows(pgxmock.NewRows([]string{"parent_id", "depth"}).
			AddRow(ptrStr("pg-root"), int(1)))
	// Reparent children: parent_id = the deleted page's parent;
	// depth shifts down by 1.
	pool.ExpectExec(`UPDATE pages SET parent_id`).
		WithArgs(ptrStr("pg-root"), "pg-mid").
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	// Delete the page.
	pool.ExpectExec(`DELETE FROM pages WHERE id`).
		WithArgs("pg-mid").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := store.Delete(context.Background(), "pg-mid"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// ─── 7. RecordView ─────────────────────────────────────────

func TestRecordView_IncrementsCount(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`UPDATE pages SET view_count`).
		WithArgs("pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := store.RecordView(context.Background(), "pg-1", "viewer-1"); err != nil {
		t.Fatalf("RecordView: %v", err)
	}
}

// ─── 8. Verify ─────────────────────────────────────────────

func TestVerify_SetsTimestampAndOwner(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`UPDATE pages SET last_verified_at`).
		WithArgs("verifier-1", "pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := store.Verify(context.Background(), "pg-1", "verifier-1"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// ─── 9. GetStalePages ──────────────────────────────────────

func TestGetStalePages_FilterByTTL(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`stale_after_days > 0`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows(pageCols()).
			AddRow(rowVals("pg-stale", "Stale doc", "stale-doc", 0, nil)...))
	out, err := store.GetStalePages(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("GetStalePages: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("got %d, want 1", len(out))
	}
}

// ─── 10. Search ────────────────────────────────────────────

func TestSearch_FullTextMatch(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`websearch_to_tsquery`).
		WithArgs("ws-1", "auth flow", 10).
		WillReturnRows(pgxmock.NewRows(pageCols()).
			AddRow(rowVals("pg-1", "Auth flow", "auth-flow", 0, nil)...))
	out, err := store.Search(context.Background(), "ws-1", "auth flow", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 || !strings.Contains(out[0].Title, "Auth") {
		t.Errorf("got %+v", out)
	}
}

// ─── 10b. SearchWithRank ───────────────────────────────────

func TestSearchWithRank_ReturnsRankedResultsWithHeadline(t *testing.T) {
	store, pool := newMockStore(t)

	cols := append(pageCols(), "space_name", "rank", "headline")
	now := time.Now().UTC()
	row1 := []driverValue{
		"pg-1", "sp-1", "ws-1", (*string)(nil), "Auth flow", "auth-flow",
		"{}", "auth flow body", "", "",
		float64(0), 0, false, "creator", "creator",
		[]string{}, float64(0),
		0, (*time.Time)(nil),
		(*time.Time)(nil), (*string)(nil), 0,
		"draft",
		false, (*string)(nil), (*time.Time)(nil),
		"document",
		now, now,
		"Engineering", float64(0.93), "Some <mark>auth</mark> flow excerpt",
	}
	row2 := []driverValue{
		"pg-2", "sp-1", "ws-1", (*string)(nil), "OAuth design", "oauth",
		"{}", "oauth doc", "", "",
		float64(0), 0, false, "creator", "creator",
		[]string{}, float64(0),
		0, (*time.Time)(nil),
		(*time.Time)(nil), (*string)(nil), 0,
		"draft",
		false, (*string)(nil), (*time.Time)(nil),
		"document",
		now, now,
		"Engineering", float64(0.42), "<mark>OAuth</mark> design",
	}

	pool.ExpectQuery(`ts_rank.*setweight.*websearch_to_tsquery`).
		WithArgs("ws-1", "auth flow", (*string)(nil), 10, 0).
		WillReturnRows(pgxmock.NewRows(cols).AddRow(row1...).AddRow(row2...))

	out, err := store.SearchWithRank(context.Background(), "ws-1", "auth flow", nil, 10, 0)
	if err != nil {
		t.Fatalf("SearchWithRank: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 results, got %d", len(out))
	}
	if out[0].Page.ID != "pg-1" || out[0].Rank < out[1].Rank {
		t.Fatalf("expected rank-sorted results: %+v", out)
	}
	if out[0].SpaceName != "Engineering" {
		t.Fatalf("space_name not surfaced: %q", out[0].SpaceName)
	}
	if !strings.Contains(out[0].Headline, "<mark>") {
		t.Fatalf("headline missing highlight: %q", out[0].Headline)
	}
}

func TestSearchWithRank_AppliesSpaceFilter(t *testing.T) {
	store, pool := newMockStore(t)
	sp := "sp-9"
	cols := append(pageCols(), "space_name", "rank", "headline")
	now := time.Now().UTC()
	pool.ExpectQuery(`ts_rank`).
		WithArgs("ws-1", "deploy", &sp, 5, 0).
		WillReturnRows(pgxmock.NewRows(cols))

	out, err := store.SearchWithRank(context.Background(), "ws-1", "deploy", &sp, 5, 0)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty, got %+v", out)
	}
	_ = now
}

func TestSearchWithRank_ClampsLimit(t *testing.T) {
	store, pool := newMockStore(t)
	cols := append(pageCols(), "space_name", "rank", "headline")
	pool.ExpectQuery(`ts_rank`).
		WithArgs("ws-1", "x", (*string)(nil), 50, 0).
		WillReturnRows(pgxmock.NewRows(cols))

	if _, err := store.SearchWithRank(context.Background(), "ws-1", "x", nil, 9999, 0); err != nil {
		t.Fatalf("SearchWithRank: %v", err)
	}
}

// ─── content_text extractor (unit) ─────────────────────────

func TestExtractContentText_FromProseMirror(t *testing.T) {
	doc := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Hello "},{"type":"text","text":"world"}]},{"type":"heading","content":[{"type":"text","text":"Goodbye"}]}]}`
	got := extractContentText(doc)
	if !strings.Contains(got, "Hello world") || !strings.Contains(got, "Goodbye") {
		t.Errorf("got %q", got)
	}
}

func TestExtractContentText_MalformedJSONReturnsEmpty(t *testing.T) {
	if got := extractContentText("{not json"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// silence unused if any test removed.
var _ = ptrTime
