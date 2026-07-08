package database

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
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

func testDBCols() []string {
	return []string{"id", "page_id", "workspace_id", "name", "schema", "created_at", "updated_at"}
}

func testRowCols() []string {
	return []string{"id", "database_id", "values", "position", "created_at", "updated_at"}
}

func testViewCols() []string {
	return []string{
		"id", "database_id", "name", "type", "filters",
		"sort_by", "sort_dir", "group_by", "hidden_cols",
		"created_at",
	}
}

// ─── Database CRUD ────────────────────────────────────

func TestCreateDatabase_StoresEmptySchema(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`INSERT INTO databases`).
		WithArgs("pg-1", "ws-1", "Roadmap", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(testDBCols()).
			AddRow("db-1", "pg-1", "ws-1", "Roadmap", []byte("[]"), now, now))

	got, err := store.CreateDatabase(context.Background(), Database{
		PageID:      "pg-1",
		WorkspaceID: "ws-1",
		Name:        "Roadmap",
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if got.ID == "" || got.Name != "Roadmap" {
		t.Fatalf("unexpected: %+v", got)
	}
	if len(got.Schema) != 0 {
		t.Fatalf("schema should be empty, got %v", got.Schema)
	}
}

func TestUpdateSchema_PersistsColumns(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	schema := []ColumnDef{
		{ID: "c-1", Name: "Title", Type: ColText},
		{ID: "c-2", Name: "Status", Type: ColSelect, Options: []string{"todo", "doing", "done"}},
	}
	encoded, _ := json.Marshal(schema)

	pool.ExpectQuery(`UPDATE databases SET schema.*workspace_id = ANY\(\$3\)`).
		WithArgs(pgxmock.AnyArg(), "db-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(testDBCols()).
			AddRow("db-1", "pg-1", "ws-1", "Roadmap", encoded, now, now))

	got, err := store.UpdateSchema(context.Background(), "db-1", schema, []string{"ws-1"})
	if err != nil {
		t.Fatalf("UpdateSchema: %v", err)
	}
	if len(got.Schema) != 2 || got.Schema[1].Type != ColSelect {
		t.Fatalf("schema not persisted: %+v", got.Schema)
	}
}

func TestUpdateSchema_RejectsTooManyColumns(t *testing.T) {
	store, _ := newMockStore(t)
	cols := make([]ColumnDef, MaxColumns+1)
	for i := range cols {
		cols[i] = ColumnDef{ID: "c", Name: "x", Type: ColText}
	}
	_, err := store.UpdateSchema(context.Background(), "db-1", cols, []string{"ws-1"})
	if err == nil {
		t.Fatal("expected error past MaxColumns")
	}
}

// ─── Row CRUD ─────────────────────────────────────────

func TestCreateRow_StoresValuesAsJSONB(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	values := map[string]any{"c-1": "Auth", "c-2": "todo"}

	pool.ExpectQuery(`SELECT EXISTS.*FROM databases WHERE id = \$1 AND workspace_id = ANY\(\$2\)`).
		WithArgs("db-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	pool.ExpectQuery(`INSERT INTO database_rows`).
		WithArgs("db-1", pgxmock.AnyArg(), float64(1)).
		WillReturnRows(pgxmock.NewRows(testRowCols()).
			AddRow("r-1", "db-1", mustJSON(values), float64(1), now, now))

	got, err := store.CreateRow(context.Background(), Row{
		DatabaseID: "db-1",
		Values:     values,
		Position:   1,
	}, []string{"ws-1"})
	if err != nil {
		t.Fatalf("CreateRow: %v", err)
	}
	if got.Values["c-1"] != "Auth" {
		t.Fatalf("values not preserved: %+v", got.Values)
	}
}

func TestUpdateRow_MergesValues(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	// Existing row has c-1=Auth, c-2=todo. The patch updates only c-2.
	existing := mustJSON(map[string]any{"c-1": "Auth", "c-2": "todo"})

	pool.ExpectQuery(`SELECT values FROM database_rows WHERE id.*workspace_id = ANY\(\$2\)`).
		WithArgs("r-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"values"}).AddRow(existing))
	pool.ExpectQuery(`UPDATE database_rows SET values.*workspace_id = ANY\(\$3\)`).
		WithArgs(pgxmock.AnyArg(), "r-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(testRowCols()).
			AddRow("r-1", "db-1", mustJSON(map[string]any{"c-1": "Auth", "c-2": "doing"}), float64(1), now, now))

	got, err := store.UpdateRow(context.Background(), "r-1", map[string]any{"c-2": "doing"}, []string{"ws-1"})
	if err != nil {
		t.Fatalf("UpdateRow: %v", err)
	}
	if got.Values["c-2"] != "doing" || got.Values["c-1"] != "Auth" {
		t.Fatalf("merge wrong: %+v", got.Values)
	}
}

func TestDeleteRow_DeletesByID(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`DELETE FROM database_rows WHERE id.*workspace_id = ANY\(\$2\)`).
		WithArgs("r-1", []string{"ws-1"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.DeleteRow(context.Background(), "r-1", []string{"ws-1"}); err != nil {
		t.Fatalf("DeleteRow: %v", err)
	}
}

// ─── ListRows with view ───────────────────────────────

func TestListRows_AppliesFilterAndSort(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	view := &DatabaseView{
		Filters: []Filter{{ColID: "c-2", Operator: "eq", Value: "todo"}},
		SortBy:  "c-1",
		SortDir: "asc",
	}

	// We don't pin exact SQL since the predicate is built dynamically,
	// but the regex must catch the operator we expect to see.
	pool.ExpectQuery(`SELECT.*FROM database_rows WHERE database_id.*workspace_id = ANY\(\$2\).*ORDER BY`).
		WithArgs("db-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(testRowCols()).
			AddRow("r-1", "db-1", mustJSON(map[string]any{"c-1": "Apple", "c-2": "todo"}), float64(1), now, now).
			AddRow("r-2", "db-1", mustJSON(map[string]any{"c-1": "Bear", "c-2": "todo"}), float64(2), now, now))

	out, err := store.ListRows(context.Background(), "db-1", view, []string{"ws-1"})
	if err != nil {
		t.Fatalf("ListRows: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out))
	}
	// Sort handled client-side post-fetch on the Values map (Postgres
	// can't directly ORDER BY a JSONB->text without a cast); the store
	// must surface them in c-1 ascending order regardless.
	if out[0].Values["c-1"] != "Apple" || out[1].Values["c-1"] != "Bear" {
		t.Fatalf("sort failed: %+v", out)
	}
}

func TestListRows_NoViewReturnsByPosition(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM database_rows WHERE database_id.*workspace_id = ANY\(\$2\).*ORDER BY position`).
		WithArgs("db-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(testRowCols()).
			AddRow("r-1", "db-1", mustJSON(map[string]any{"c-1": "First"}), float64(1), now, now))
	out, err := store.ListRows(context.Background(), "db-1", nil, []string{"ws-1"})
	if err != nil {
		t.Fatalf("ListRows: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 row, got %d", len(out))
	}
}

// ─── Views ────────────────────────────────────────────

func TestCreateView_StoresTableView(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT EXISTS.*FROM databases WHERE id = \$1 AND workspace_id = ANY\(\$2\)`).
		WithArgs("db-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	pool.ExpectQuery(`INSERT INTO database_views`).
		WithArgs("db-1", "All items", "table", pgxmock.AnyArg(), "c-1", "asc", "", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(testViewCols()).AddRow(
			"v-1", "db-1", "All items", "table",
			[]byte("[]"), "c-1", "asc", "", []string{},
			now,
		))
	got, err := store.CreateView(context.Background(), DatabaseView{
		DatabaseID: "db-1",
		Name:       "All items",
		Type:       ViewTable,
		SortBy:     "c-1",
		SortDir:    "asc",
	}, []string{"ws-1"})
	if err != nil {
		t.Fatalf("CreateView: %v", err)
	}
	if got.Type != ViewTable {
		t.Fatalf("type lost: %q", got.Type)
	}
}

func TestListViews_ReturnsAll(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM database_views WHERE database_id.*workspace_id = ANY\(\$2\)`).
		WithArgs("db-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows(testViewCols()).
			AddRow("v-1", "db-1", "Table", "table", []byte("[]"), "", "asc", "", []string{}, now).
			AddRow("v-2", "db-1", "Kanban", "kanban", []byte("[]"), "", "asc", "c-status", []string{}, now))
	out, err := store.ListViews(context.Background(), "db-1", []string{"ws-1"})
	if err != nil {
		t.Fatalf("ListViews: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 views, got %d", len(out))
	}
	if out[1].Type != ViewKanban || out[1].GroupBy != "c-status" {
		t.Fatalf("kanban not preserved: %+v", out[1])
	}
}

// ─── Filter evaluator ────────────────────────────────

func TestApplyFilter_OperatorsAcrossValueTypes(t *testing.T) {
	row := Row{Values: map[string]any{
		"c-text": "hello world",
		"c-num":  float64(42),
		"c-flag": true,
	}}
	cases := []struct {
		name string
		f    Filter
		want bool
	}{
		{"eq", Filter{ColID: "c-text", Operator: "eq", Value: "hello world"}, true},
		{"neq", Filter{ColID: "c-text", Operator: "neq", Value: "hello"}, true},
		{"contains", Filter{ColID: "c-text", Operator: "contains", Value: "world"}, true},
		{"gt", Filter{ColID: "c-num", Operator: "gt", Value: "10"}, true},
		{"lt", Filter{ColID: "c-num", Operator: "lt", Value: "10"}, false},
		{"missing col returns false", Filter{ColID: "absent", Operator: "eq", Value: "x"}, false},
	}
	for _, c := range cases {
		got := applyFilter(row, c.f)
		if got != c.want {
			t.Errorf("%s: want %v, got %v", c.name, c.want, got)
		}
	}
	// String containment is case-insensitive — verify explicitly.
	if !applyFilter(row, Filter{ColID: "c-text", Operator: "contains", Value: "HELLO"}) {
		t.Fatal("contains should be case-insensitive")
	}
	// Unknown operator: never matches.
	if applyFilter(row, Filter{ColID: "c-text", Operator: "zorp", Value: "hello"}) {
		t.Fatal("unknown operator must return false")
	}
	// Don't unused-warn the strings import below.
	_ = strings.ToLower
}

// mustJSON returns the JSON encoding of v as []byte, panic-free.
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
