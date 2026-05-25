package changelog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/trackintegration"
)

func newMockStore(t *testing.T, track issueLookup) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return newStore(pool, track), pool
}

func entryCols() []string {
	return []string{
		"id", "page_id", "workspace_id", "version", "title", "summary",
		"type", "issue_ids", "content", "published_at",
		"created_by", "created_at", "updated_at",
	}
}

// fakeTrack stubs the Track client. Tests configure issues by ID;
// IsConfigured can be flipped to simulate Track being unavailable.
type fakeTrack struct {
	configured bool
	issues     map[string]*trackintegration.IssueRef
}

func (f *fakeTrack) IsConfigured() bool { return f.configured }
func (f *fakeTrack) GetIssue(_ context.Context, _, id string) (*trackintegration.IssueRef, error) {
	return f.issues[id], nil
}

// ─── Create / Update / Delete ────────────────────────

func TestCreateEntry_StoresFields(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	now := time.Now().UTC()

	pool.ExpectQuery(`INSERT INTO changelog_entries`).
		WithArgs("pg-1", "ws-1", "v2.1.0", "Auth refresh", "Summary",
			"feature", []string{"ENG-1", "ENG-2"}, "{}", "u-author").
		WillReturnRows(pgxmock.NewRows(entryCols()).AddRow(
			"e-1", "pg-1", "ws-1", "v2.1.0", "Auth refresh", "Summary",
			"feature", []string{"ENG-1", "ENG-2"}, "{}", (*time.Time)(nil),
			"u-author", now, now,
		))

	out, err := store.CreateEntry(context.Background(), ChangelogEntry{
		PageID:      "pg-1",
		WorkspaceID: "ws-1",
		Version:     "v2.1.0",
		Title:       "Auth refresh",
		Summary:     "Summary",
		Type:        EntryFeature,
		IssueIDs:    []string{"ENG-1", "ENG-2"},
		Content:     "{}",
		CreatedBy:   "u-author",
	})
	if err != nil {
		t.Fatalf("CreateEntry: %v", err)
	}
	if out.ID != "e-1" || out.Version != "v2.1.0" {
		t.Fatalf("unexpected: %+v", out)
	}
}

func TestCreateEntry_RejectsBadVersion(t *testing.T) {
	store, _ := newMockStore(t, &fakeTrack{})
	_, err := store.CreateEntry(context.Background(), ChangelogEntry{
		PageID:      "pg-1",
		WorkspaceID: "ws-1",
		Version:     "release!!!",
		Title:       "X",
		Type:        EntryFeature,
	})
	if err == nil {
		t.Fatal("expected version validation error")
	}
}

func TestCreateEntry_AcceptsSemverAndDates(t *testing.T) {
	for _, v := range []string{"v1.2.3", "1.2.3", "v0.1.0-rc1", "2026-05-23"} {
		if !isValidVersion(v) {
			t.Errorf("expected %q valid", v)
		}
	}
	for _, v := range []string{"", "release!", "abc.def"} {
		if isValidVersion(v) {
			t.Errorf("expected %q invalid", v)
		}
	}
}

func TestPublishEntry_SetsTimestamp(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	now := time.Now().UTC()
	pool.ExpectQuery(`UPDATE changelog_entries SET published_at`).
		WithArgs("e-1").
		WillReturnRows(pgxmock.NewRows(entryCols()).AddRow(
			"e-1", "pg-1", "ws-1", "v2.1.0", "Auth refresh", "Summary",
			"feature", []string{}, "{}", &now,
			"u-author", now, now,
		))
	out, err := store.PublishEntry(context.Background(), "e-1")
	if err != nil {
		t.Fatalf("PublishEntry: %v", err)
	}
	if out.PublishedAt == nil {
		t.Fatal("expected published_at to be set")
	}
}

func TestDeleteEntry_DeletesByID(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	pool.ExpectExec(`DELETE FROM changelog_entries WHERE id`).
		WithArgs("e-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.DeleteEntry(context.Background(), "e-1"); err != nil {
		t.Fatalf("DeleteEntry: %v", err)
	}
}

// ─── ListEntries ─────────────────────────────────────

func TestListEntries_OrdersByPublishedDescThenCreated(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	pool.ExpectQuery(`SELECT.*FROM changelog_entries WHERE page_id.*ORDER BY published_at DESC NULLS LAST, created_at DESC`).
		WithArgs("pg-1", 20, 0).
		WillReturnRows(pgxmock.NewRows(entryCols()))
	_, err := store.ListEntries(context.Background(), "pg-1", nil, 20, 0)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
}

func TestListEntries_FiltersByType(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	pool.ExpectQuery(`SELECT.*FROM changelog_entries WHERE page_id.*type = `).
		WithArgs("pg-1", "feature", 20, 0).
		WillReturnRows(pgxmock.NewRows(entryCols()))
	tp := EntryFeature
	if _, err := store.ListEntries(context.Background(), "pg-1", &tp, 20, 0); err != nil {
		t.Fatalf("ListEntries by type: %v", err)
	}
}

// ─── GenerateFromIssues ──────────────────────────────

func TestGenerateFromIssues_GroupsByLabel(t *testing.T) {
	track := &fakeTrack{
		configured: true,
		issues: map[string]*trackintegration.IssueRef{
			"i-1": {ID: "i-1", Identifier: "ENG-1", Title: "Auth bug", Labels: []string{"bug"}},
			"i-2": {ID: "i-2", Identifier: "ENG-2", Title: "Dark mode", Labels: []string{"feature"}},
			"i-3": {ID: "i-3", Identifier: "ENG-3", Title: "Drop v1 API", Labels: []string{"breaking-change"}},
		},
	}
	store, pool := newMockStore(t, track)
	now := time.Now().UTC()

	// Mix contains bug + feature + breaking → the chosen entry-type
	// is `breaking` (highest precedence). We accept any args for
	// title/summary/content because those are generated server-side.
	pool.ExpectQuery(`INSERT INTO changelog_entries`).
		WithArgs(
			"pg-1", "ws-1", "v2.1.0",
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			"breaking",
			[]string{"i-1", "i-2", "i-3"},
			pgxmock.AnyArg(),
			"u-author",
		).
		WillReturnRows(pgxmock.NewRows(entryCols()).AddRow(
			"e-1", "pg-1", "ws-1", "v2.1.0", "v2.1.0", "Generated from 3 issues",
			"breaking", []string{"i-1", "i-2", "i-3"}, "{...content...}", (*time.Time)(nil),
			"u-author", now, now,
		))

	out, err := store.GenerateFromIssues(context.Background(),
		"ws-1", "pg-1", "u-author",
		[]string{"i-1", "i-2", "i-3"}, "v2.1.0")
	if err != nil {
		t.Fatalf("GenerateFromIssues: %v", err)
	}
	if out == nil {
		t.Fatal("expected entry")
	}
	// The store should call CreateEntry exactly once with a
	// pre-built ProseMirror content blob — sniff via the args
	// expectation.
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestGenerateFromIssues_BuildContentGroupsHeadings(t *testing.T) {
	track := &fakeTrack{
		configured: true,
		issues: map[string]*trackintegration.IssueRef{
			"i-1": {ID: "i-1", Identifier: "ENG-1", Title: "Auth bug", Labels: []string{"bug"}},
			"i-2": {ID: "i-2", Identifier: "ENG-2", Title: "Dark mode", Labels: []string{"feature"}},
		},
	}
	content := buildContent(track, []string{"i-1", "i-2"})
	// Content should contain group headings for both buckets.
	if !strings.Contains(content, "Bug Fixes") {
		t.Fatalf("missing bugfix heading: %q", content)
	}
	if !strings.Contains(content, "New Features") {
		t.Fatalf("missing feature heading: %q", content)
	}
	// Bullets should reference the Track identifiers.
	if !strings.Contains(content, "ENG-1") || !strings.Contains(content, "ENG-2") {
		t.Fatalf("missing identifiers in body: %q", content)
	}
}

func TestGenerateFromIssues_GracefulWhenTrackUnavailable(t *testing.T) {
	track := &fakeTrack{configured: false}
	store, pool := newMockStore(t, track)
	now := time.Now().UTC()

	// Track unavailable → entry still created with the supplied
	// issue IDs but with a placeholder content blob and improvement
	// category.
	pool.ExpectQuery(`INSERT INTO changelog_entries`).
		WithArgs(
			"pg-1", "ws-1", "v2.1.0",
			pgxmock.AnyArg(), pgxmock.AnyArg(),
			"improvement",
			[]string{"i-1"},
			pgxmock.AnyArg(),
			"u-author",
		).
		WillReturnRows(pgxmock.NewRows(entryCols()).AddRow(
			"e-1", "pg-1", "ws-1", "v2.1.0", "v2.1.0", "",
			"improvement", []string{"i-1"}, "{}", (*time.Time)(nil),
			"u-author", now, now,
		))

	if _, err := store.GenerateFromIssues(context.Background(),
		"ws-1", "pg-1", "u-author", []string{"i-1"}, "v2.1.0"); err != nil {
		t.Fatalf("GenerateFromIssues (track down): %v", err)
	}
}

// ─── GetPublicFeed ───────────────────────────────────

func TestGetPublicFeed_OnlyReturnsPublished(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	pool.ExpectQuery(`SELECT.*FROM changelog_entries WHERE workspace_id.*published_at IS NOT NULL`).
		WithArgs("ws-1", 10).
		WillReturnRows(pgxmock.NewRows(entryCols()))
	_, err := store.GetPublicFeed(context.Background(), "ws-1", 10)
	if err != nil {
		t.Fatalf("GetPublicFeed: %v", err)
	}
}

func TestGetPublicFeed_ClampsLimit(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	pool.ExpectQuery(`FROM changelog_entries WHERE workspace_id`).
		WithArgs("ws-1", 100).
		WillReturnRows(pgxmock.NewRows(entryCols()))
	if _, err := store.GetPublicFeed(context.Background(), "ws-1", 9999); err != nil {
		t.Fatalf("GetPublicFeed: %v", err)
	}
}
