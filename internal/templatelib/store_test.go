package templatelib

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/model"
)

// fakePages stubs the page creation surface so UseTemplate tests
// don't have to mock the full page-store SQL. Records every Create
// call.
type fakePages struct {
	mu      sync.Mutex
	created []model.Page
}

func (f *fakePages) Create(_ context.Context, p model.Page) (*model.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p.ID = "pg-from-template"
	f.created = append(f.created, p)
	return &p, nil
}

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface, *fakePages) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	pages := &fakePages{}
	return newStore(pool, pages), pool, pages
}

func libCols() []string {
	return []string{
		"id", "name", "description", "category", "icon",
		"tags", "content", "content_text", "is_built_in",
		"workspace_id", "created_by", "use_count", "created_at",
	}
}

// ─── List ─────────────────────────────────────────────

func TestList_ReturnsAllBuiltins_NoDBRows(t *testing.T) {
	store, pool, _ := newMockStore(t)
	pool.ExpectQuery(`SELECT.*FROM library_templates WHERE workspace_id = ANY`).
		WithArgs([]string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(libCols()))

	out, err := store.List(context.Background(), []string{"ws-1"}, nil, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != BuiltinCount {
		t.Fatalf("want %d built-ins, got %d", BuiltinCount, len(out))
	}
	for _, tmpl := range out {
		if !tmpl.IsBuiltIn {
			t.Fatalf("expected all built-in, got %q is_built_in=%v", tmpl.Name, tmpl.IsBuiltIn)
		}
	}
}

func TestList_MergesWorkspaceAndBuiltins(t *testing.T) {
	store, pool, _ := newMockStore(t)
	wsID := "ws-1"
	customCreatedBy := "alice"
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM library_templates WHERE workspace_id = ANY`).
		WithArgs([]string{wsID}).
		WillReturnRows(pgxmock.NewRows(libCols()).AddRow(
			"t-1", "Custom RFC", "internal version", "engineering", "📝",
			[]string{"rfc"}, "{}", "Custom body", false,
			&wsID, &customCreatedBy, 3, now,
		))
	out, err := store.List(context.Background(), []string{wsID}, nil, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != BuiltinCount+1 {
		t.Fatalf("want %d total, got %d", BuiltinCount+1, len(out))
	}
	var foundCustom bool
	for _, tmpl := range out {
		if tmpl.ID == "t-1" {
			foundCustom = true
			if tmpl.IsBuiltIn {
				t.Fatalf("custom template flagged as built-in: %+v", tmpl)
			}
		}
	}
	if !foundCustom {
		t.Fatalf("custom template missing from List output")
	}
}

func TestList_FiltersByCategory(t *testing.T) {
	store, pool, _ := newMockStore(t)
	pool.ExpectQuery(`SELECT.*FROM library_templates WHERE workspace_id = ANY`).
		WithArgs([]string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(libCols()))

	cat := CatEngineering
	out, err := store.List(context.Background(), []string{"ws-1"}, &cat, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected engineering templates, got none")
	}
	for _, tmpl := range out {
		if tmpl.Category != CatEngineering {
			t.Fatalf("category filter leaked %q: %+v", tmpl.Category, tmpl)
		}
	}
}

func TestList_FiltersBySearchQuery(t *testing.T) {
	store, pool, _ := newMockStore(t)
	pool.ExpectQuery(`SELECT.*FROM library_templates WHERE workspace_id = ANY`).
		WithArgs([]string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(libCols()))

	out, err := store.List(context.Background(), []string{"ws-1"}, nil, "RFC")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected RFC matches, got none")
	}
	// Every result must mention the search term in name OR description.
	for _, tmpl := range out {
		lower := strings.ToLower(tmpl.Name + " " + tmpl.Description)
		if !strings.Contains(lower, "rfc") {
			t.Fatalf("template %q matched search but has no rfc: %+v", tmpl.Name, tmpl)
		}
	}
}

// ─── CreateFromPage ───────────────────────────────────

func TestCreateFromPage_StoresWorkspaceTemplate(t *testing.T) {
	store, pool, _ := newMockStore(t)
	now := time.Now().UTC()

	// 1. Look up page content + content_text (scoped to the caller's set).
	pool.ExpectQuery(`SELECT content, content_text FROM pages WHERE id = \$1 AND workspace_id = ANY`).
		WithArgs("pg-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"content", "content_text"}).
			AddRow(`{"type":"doc","content":[{"type":"paragraph"}]}`, "hello"))
	// 2. Insert library row.
	wsID := "ws-1"
	creator := "alice"
	pool.ExpectQuery(`INSERT INTO library_templates`).
		WithArgs("Standup notes", "Daily standup template", "operations",
			"📄", []string{}, pgxmock.AnyArg(), "hello", false, &wsID, &creator).
		WillReturnRows(pgxmock.NewRows(libCols()).AddRow(
			"t-1", "Standup notes", "Daily standup template", "operations", "📄",
			[]string{}, `{"type":"doc"}`, "hello", false,
			&wsID, &creator, 0, now,
		))

	got, err := store.CreateFromPage(context.Background(),
		"pg-1", "ws-1", "alice",
		"Standup notes", "Daily standup template",
		CatOperations, []string{"ws-1"})
	if err != nil {
		t.Fatalf("CreateFromPage: %v", err)
	}
	if got.ID != "t-1" || got.IsBuiltIn {
		t.Fatalf("unexpected: %+v", got)
	}
}

// ─── UseTemplate ──────────────────────────────────────

func TestUseTemplate_FromBuiltin_CreatesPageAndCounts(t *testing.T) {
	store, _, pages := newMockStore(t)

	// Find a built-in to use.
	builtins := Builtins()
	if len(builtins) == 0 {
		t.Fatal("no built-in templates loaded")
	}
	t1 := builtins[0]

	got, err := store.UseTemplate(context.Background(), t1.ID, "sp-1", "ws-1", "bob", nil)
	if err != nil {
		t.Fatalf("UseTemplate: %v", err)
	}
	if got == nil || got.Title != t1.Name {
		t.Fatalf("page wrong: %+v", got)
	}
	if len(pages.created) != 1 {
		t.Fatalf("want 1 page Create, got %d", len(pages.created))
	}
	// use_count for the built-in tracks via the store's in-memory map.
	if store.UseCountForBuiltin(t1.ID) != 1 {
		t.Fatalf("want use_count=1 after one use, got %d", store.UseCountForBuiltin(t1.ID))
	}
	// Use it again — counter should bump.
	if _, err := store.UseTemplate(context.Background(), t1.ID, "sp-1", "ws-1", "bob", nil); err != nil {
		t.Fatalf("UseTemplate (2nd): %v", err)
	}
	if store.UseCountForBuiltin(t1.ID) != 2 {
		t.Fatalf("want use_count=2, got %d", store.UseCountForBuiltin(t1.ID))
	}
}

func TestUseTemplate_FromCustom_IncrementsDBUseCount(t *testing.T) {
	store, pool, pages := newMockStore(t)
	now := time.Now().UTC()
	wsID := "ws-1"

	// Lookup the custom template (scoped to the caller's verified set).
	pool.ExpectQuery(`SELECT.*FROM library_templates WHERE id = \$1 AND workspace_id = ANY`).
		WithArgs("t-custom", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(libCols()).AddRow(
			"t-custom", "Custom doc", "", "general", "📄",
			[]string{}, `{"type":"doc","content":[{"type":"paragraph"}]}`, "body",
			false, &wsID, ptrStr("alice"), 0, now,
		))
	// Then bump use_count (same scope).
	pool.ExpectExec(`UPDATE library_templates SET use_count.*WHERE id = \$1 AND workspace_id = ANY`).
		WithArgs("t-custom", []string{"ws-1"}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	got, err := store.UseTemplate(context.Background(), "t-custom", "sp-1", "ws-1", "bob", []string{"ws-1"})
	if err != nil {
		t.Fatalf("UseTemplate: %v", err)
	}
	if got == nil {
		t.Fatal("page not returned")
	}
	if len(pages.created) != 1 {
		t.Fatalf("want 1 page, got %d", len(pages.created))
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── Delete ───────────────────────────────────────────

func TestDelete_AllowsWorkspaceTemplate(t *testing.T) {
	store, pool, _ := newMockStore(t)
	pool.ExpectExec(`DELETE FROM library_templates WHERE id = \$1 AND workspace_id = ANY`).
		WithArgs("t-1", []string{"ws-1"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.Delete(context.Background(), "t-1", []string{"ws-1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDelete_RejectsBuiltin(t *testing.T) {
	store, _, _ := newMockStore(t)
	builtins := Builtins()
	if len(builtins) == 0 {
		t.Fatal("no built-ins")
	}
	if err := store.Delete(context.Background(), builtins[0].ID, []string{"ws-1"}); err == nil {
		t.Fatal("expected error deleting built-in")
	}
}

// ─── Helpers ──────────────────────────────────────────

func ptrStr(s string) *string { return &s }
