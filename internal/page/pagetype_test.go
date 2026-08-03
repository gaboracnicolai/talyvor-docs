package page

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/testutil"
)

// pagetype_test.go — pages.page_type.
//
// THE STATE THIS FIXES. The column shipped in 0012_changelog.sql with a NOT NULL DEFAULT
// 'document'. It is SELECTed by this store, carried on model.Page, serialised as `page_type`,
// and branched on by the frontend in two places (PageView routes to ChangelogView, Sidebar
// picks the icon). Nothing ever WROTE it. So every row held 'document' forever, the frontend's
// `page.page_type === "changelog"` was unreachable by construction, and the whole changelog
// feature — eight mounted routes, a store, four React components — had no way in.
//
// Its doc comment on model.Page also claimed a consumer it did not have ("the changelog package
// reads this"; that package has never mentioned it) and a third value, "template", that nothing
// produces — `is_template` is the actual template mechanism and is a separate boolean column.

func newPageStore(t *testing.T, d *testutil.DB) *Store {
	t.Helper()
	return NewStore(d.Pool)
}

// seedSpace returns a space id to create pages under. testutil.DB.Page makes a space too but
// only hands back the page id, and these cases exercise Create itself.
func seedSpace(t *testing.T, d *testutil.DB, wsID string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by)
		 VALUES ($1, 'Space', $2, 'author') RETURNING id`,
		wsID, "space-"+wsID,
	).Scan(&id); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return id
}

// A page created as a changelog stays a changelog. Without a writer this is the assertion the
// frontend has been waiting on since 0012.
func TestCreate_PersistsPageType(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	spaceID := seedSpace(t, d, ws)
	s := newPageStore(t, d)

	out, err := s.Create(ctx, model.Page{
		SpaceID: spaceID, WorkspaceID: ws, CreatedBy: "author",
		Title: "Release notes", PageType: "changelog",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.PageType != "changelog" {
		t.Fatalf("Create returned page_type=%q, want \"changelog\" — nothing writes the column, so "+
			"the frontend's ChangelogView is unreachable no matter what a caller asks for", out.PageType)
	}
	// Round-tripped, not just echoed back from the struct we passed in.
	got, err := s.GetByID(ctx, out.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PageType != "changelog" {
		t.Fatalf("re-read page_type=%q, want \"changelog\" — the value did not reach the column", got.PageType)
	}
}

// The default survives: an ordinary create is still a document.
func TestCreate_PageTypeDefaultsToDocument(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	s := newPageStore(t, d)

	out, err := s.Create(ctx, model.Page{
		SpaceID: seedSpace(t, d, ws), WorkspaceID: ws, CreatedBy: "author", Title: "Spec",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.PageType != "document" {
		t.Fatalf("page_type=%q on a plain create, want \"document\"", out.PageType)
	}
}

// An existing document can be turned into a changelog — the path that matters for the pages
// people already have.
func TestUpdate_PersistsPageType(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	pageID := d.Page(t, ws, "author", "Notes")
	s := newPageStore(t, d)

	out, err := s.Update(ctx, pageID, map[string]any{"page_type": "changelog"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.PageType != "changelog" {
		t.Fatalf("after update page_type=%q, want \"changelog\" — page_type is not in the Update "+
			"allowlist, so the patch is silently dropped and the caller gets a 200 saying otherwise",
			out.PageType)
	}
}

// ⚠ A FREE-TEXT COLUMN THE FRONTEND SWITCHES ON MUST BE CLOSED. `page_type TEXT` accepts
// anything; the frontend's union type is "document" | "changelog". An unrecognised value falls
// through every branch and renders a page that looks empty rather than wrong — so the write is
// rejected at the store, where the column is, not at one of the handlers that reach it.
func TestPageType_RejectsAnUnknownValue(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	s := newPageStore(t, d)

	if _, err := s.Create(ctx, model.Page{
		SpaceID: seedSpace(t, d, ws), WorkspaceID: ws, CreatedBy: "author",
		Title: "Odd", PageType: "wiki",
	}); err == nil {
		t.Error("Create accepted page_type=\"wiki\" — no frontend branch matches it, so the page " +
			"renders blank instead of failing the write")
	}

	pageID := d.Page(t, ws, "author", "Notes")
	if _, err := s.Update(ctx, pageID, map[string]any{"page_type": "wiki"}); err == nil {
		t.Error("Update accepted page_type=\"wiki\"")
	}

	// ⚠ "template" is NOT a third value, whatever the old comment said: `is_template` is a
	// separate boolean column and is what every template query actually reads. Accepting it
	// here would give the concept two representations that can disagree.
	if _, err := s.Update(ctx, pageID, map[string]any{"page_type": "template"}); err == nil {
		t.Error("Update accepted page_type=\"template\" — templates are is_template, and one " +
			"concept with two columns is one column too many")
	}
}
