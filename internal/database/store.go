// Package database owns the inline-database block — Notion's killer
// feature, ported to Docs. A `Database` carries a user-defined
// schema (a list of ColumnDef); rows live in `database_rows` with
// values stored as JSONB so the column set can evolve without an
// ALTER TABLE. Multiple views (table / list / kanban / gallery) can
// project the same data through different filters + sort + group-by.
package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxColumns + MaxRows match the spec constraints. Bounded so a
// runaway user (or agent) can't blow up a page render.
const (
	MaxColumns = 50
	MaxRows    = 10_000
)

// SEC-4 Layer 2: by-id ops scope to the caller's VERIFIED workspace set (resolved from
// membership by Layer 1), passed as wsIDs. `databases` carries workspace_id directly; the
// child tables (database_rows, database_views) join back to `databases` on database_id. A row
// whose owning database lives in a workspace the caller doesn't belong to is invisible →
// ErrNotFound → 404, never leaking existence. wsIDs empty (no membership) matches nothing.
// The workspace filter comes from verified membership, never a client header/body — the IDOR cure.

// ErrNotFound signals a by-id op resolved to no row IN THE CALLER'S WORKSPACES — the handler
// maps it to 404. Distinct from a raw DB error so a real failure is never masked as not-found.
var ErrNotFound = errors.New("database: not found in workspace")

// ─── Types ───────────────────────────────────────────

type ColumnType string

const (
	ColText     ColumnType = "text"
	ColNumber   ColumnType = "number"
	ColSelect   ColumnType = "select"
	ColMulti    ColumnType = "multi_select"
	ColDate     ColumnType = "date"
	ColCheckbox ColumnType = "checkbox"
	ColURL      ColumnType = "url"
	ColRelation ColumnType = "relation"
	ColFormula  ColumnType = "formula"
)

type ColumnDef struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Type    ColumnType `json:"type"`
	Options []string   `json:"options,omitempty"`
	Formula string     `json:"formula,omitempty"`
}

type Database struct {
	ID          string      `json:"id"`
	PageID      string      `json:"page_id"`
	WorkspaceID string      `json:"workspace_id"`
	Name        string      `json:"name"`
	Schema      []ColumnDef `json:"schema"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type Row struct {
	ID         string         `json:"id"`
	DatabaseID string         `json:"database_id"`
	Values     map[string]any `json:"values"`
	Position   float64        `json:"position"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type ViewType string

const (
	ViewTable   ViewType = "table"
	ViewList    ViewType = "list"
	ViewKanban  ViewType = "kanban"
	ViewGallery ViewType = "gallery"
)

type Filter struct {
	ColID    string `json:"col_id"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
}

type DatabaseView struct {
	ID         string    `json:"id"`
	DatabaseID string    `json:"database_id"`
	Name       string    `json:"name"`
	Type       ViewType  `json:"type"`
	Filters    []Filter  `json:"filters"`
	SortBy     string    `json:"sort_by"`
	SortDir    string    `json:"sort_dir"`
	GroupBy    string    `json:"group_by,omitempty"`
	HiddenCols []string  `json:"hidden_cols"`
	CreatedAt  time.Time `json:"created_at"`
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct{ pool pgxDB }

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(db pgxDB) *Store { return &Store{pool: db} }

// ─── Database CRUD ──────────────────────────────────

const dbCols = `id, page_id, workspace_id, name, schema, created_at, updated_at`

func scanDatabase(s interface{ Scan(...any) error }) (*Database, error) {
	var (
		d         Database
		rawSchema []byte
	)
	if err := s.Scan(&d.ID, &d.PageID, &d.WorkspaceID, &d.Name, &rawSchema, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if len(rawSchema) > 0 {
		_ = json.Unmarshal(rawSchema, &d.Schema)
	}
	if d.Schema == nil {
		d.Schema = []ColumnDef{}
	}
	return &d, nil
}

func (s *Store) CreateDatabase(ctx context.Context, d Database) (*Database, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	if d.PageID == "" {
		return nil, errors.New("database: page_id required")
	}
	if d.Name == "" {
		d.Name = "Untitled Database"
	}
	if d.Schema == nil {
		d.Schema = []ColumnDef{}
	}
	if err := validateSchema(d.Schema); err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(d.Schema)
	row := s.pool.QueryRow(ctx,
		`INSERT INTO databases (page_id, workspace_id, name, schema)
        VALUES ($1, $2, $3, $4)
        RETURNING `+dbCols,
		d.PageID, d.WorkspaceID, d.Name, encoded,
	)
	return scanDatabase(row)
}

// assertDatabaseInWorkspaces returns ErrNotFound unless database id lives in one of wsIDs. Used
// by the child-table ops (rows/views) to gate an INSERT keyed by a caller-supplied database_id.
func (s *Store) assertDatabaseInWorkspaces(ctx context.Context, databaseID string, wsIDs []string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM databases WHERE id = $1 AND workspace_id = ANY($2))`,
		databaseID, wsIDs,
	).Scan(&exists); err != nil {
		return fmt.Errorf("database: scope check: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetDatabase(ctx context.Context, id string, wsIDs []string) (*Database, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+dbCols+` FROM databases WHERE id = $1 AND workspace_id = ANY($2)`, id, wsIDs)
	d, err := scanDatabase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) UpdateSchema(ctx context.Context, id string, schema []ColumnDef, wsIDs []string) (*Database, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	if err := validateSchema(schema); err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(schema)
	row := s.pool.QueryRow(ctx,
		`UPDATE databases SET schema = $1, updated_at = NOW() WHERE id = $2 AND workspace_id = ANY($3) RETURNING `+dbCols,
		encoded, id, wsIDs,
	)
	d, err := scanDatabase(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func validateSchema(cols []ColumnDef) error {
	if len(cols) > MaxColumns {
		return fmt.Errorf("database: schema exceeds MaxColumns (%d)", MaxColumns)
	}
	return nil
}

// ─── Row CRUD ───────────────────────────────────────

const rowCols = `id, database_id, values, position, created_at, updated_at`

func scanRow(s interface{ Scan(...any) error }) (*Row, error) {
	var (
		r         Row
		rawValues []byte
	)
	if err := s.Scan(&r.ID, &r.DatabaseID, &rawValues, &r.Position, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Values = map[string]any{}
	if len(rawValues) > 0 {
		_ = json.Unmarshal(rawValues, &r.Values)
	}
	return &r, nil
}

func (s *Store) CreateRow(ctx context.Context, r Row, wsIDs []string) (*Row, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	if r.DatabaseID == "" {
		return nil, errors.New("database: database_id required")
	}
	// A member of A can't add rows to B's database: the target database must live in the
	// caller's verified workspace set.
	if err := s.assertDatabaseInWorkspaces(ctx, r.DatabaseID, wsIDs); err != nil {
		return nil, err
	}
	if r.Values == nil {
		r.Values = map[string]any{}
	}
	encoded, _ := json.Marshal(r.Values)
	row := s.pool.QueryRow(ctx,
		`INSERT INTO database_rows (database_id, values, position)
        VALUES ($1, $2, $3)
        RETURNING `+rowCols,
		r.DatabaseID, encoded, r.Position,
	)
	return scanRow(row)
}

// UpdateRow merges the patch into the existing row's value map. We
// do the merge in Go (read-modify-write) because pgx's `||` JSONB
// operator silently overwrites entire object keys and we want a
// per-cell semantics — patch{c-2: doing} should keep c-1 intact.
func (s *Store) UpdateRow(ctx context.Context, id string, patch map[string]any, wsIDs []string) (*Row, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	if patch == nil {
		patch = map[string]any{}
	}
	var existing []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT values FROM database_rows WHERE id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))`, id, wsIDs,
	).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("database: row not found: %w", err)
	}
	merged := map[string]any{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &merged)
	}
	for k, v := range patch {
		merged[k] = v
	}
	encoded, _ := json.Marshal(merged)
	row := s.pool.QueryRow(ctx,
		`UPDATE database_rows SET values = $1, updated_at = NOW() WHERE id = $2
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($3)) RETURNING `+rowCols,
		encoded, id, wsIDs,
	)
	r, err := scanRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func (s *Store) DeleteRow(ctx context.Context, id string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("database: no pool")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM database_rows WHERE id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))`, id, wsIDs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRows fetches every row for the database, then applies the
// view's filters + sort in Go. We do the post-fetch processing
// rather than building dynamic WHERE clauses against JSONB because
// the row counts are bounded (MaxRows = 10K) and the rule engine
// stays unit-testable.
func (s *Store) ListRows(ctx context.Context, databaseID string, view *DatabaseView, wsIDs []string) ([]Row, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+rowCols+` FROM database_rows WHERE database_id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))
        ORDER BY position ASC`,
		databaseID, wsIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list rows: %w", err)
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if view == nil {
		return out, nil
	}
	out = filterRows(out, view.Filters)
	if view.SortBy != "" {
		sortRows(out, view.SortBy, view.SortDir)
	}
	return out, nil
}

// filterRows keeps the rows that satisfy every filter (AND semantics).
func filterRows(rows []Row, filters []Filter) []Row {
	if len(filters) == 0 {
		return rows
	}
	kept := rows[:0]
	for _, r := range rows {
		ok := true
		for _, f := range filters {
			if !applyFilter(r, f) {
				ok = false
				break
			}
		}
		if ok {
			kept = append(kept, r)
		}
	}
	return kept
}

// applyFilter implements the operator matrix from the spec. Numeric
// comparisons coerce both sides; text compares are case-insensitive
// for "contains".
func applyFilter(r Row, f Filter) bool {
	v, ok := r.Values[f.ColID]
	if !ok {
		return false
	}
	switch f.Operator {
	case "eq":
		return cellEquals(v, f.Value)
	case "neq":
		return !cellEquals(v, f.Value)
	case "contains":
		return strings.Contains(strings.ToLower(stringOf(v)), strings.ToLower(f.Value))
	case "gt", "lt":
		a, aOK := numberOf(v)
		b, bOK := numberOf(f.Value)
		if !aOK || !bOK {
			return false
		}
		if f.Operator == "gt" {
			return a > b
		}
		return a < b
	}
	return false
}

func cellEquals(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case bool:
		return strconv.FormatBool(x) == strings.ToLower(want)
	case float64:
		w, ok := numberOf(want)
		return ok && x == w
	}
	return stringOf(v) == want
}

func stringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// sortRows orders rows by the named column. Strings use natural
// ordering; numbers + bools compare per-type. Mixed types degrade to
// stringified comparison.
func sortRows(rows []Row, colID, dir string) {
	asc := dir != "desc"
	sort.SliceStable(rows, func(i, j int) bool {
		a := rows[i].Values[colID]
		b := rows[j].Values[colID]
		less := compareValues(a, b)
		if !asc {
			less = -less
		}
		return less < 0
	})
}

func compareValues(a, b any) int {
	an, aok := numberOf(a)
	bn, bok := numberOf(b)
	if aok && bok {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		default:
			return 0
		}
	}
	as := stringOf(a)
	bs := stringOf(b)
	switch {
	case as < bs:
		return -1
	case as > bs:
		return 1
	default:
		return 0
	}
}

// ─── Views ──────────────────────────────────────────

const viewSelectCols = `id, database_id, name, type, filters, sort_by, sort_dir, group_by, hidden_cols, created_at`

func scanView(s interface{ Scan(...any) error }) (*DatabaseView, error) {
	var (
		v          DatabaseView
		rawFilters []byte
		hidden     []string
	)
	if err := s.Scan(&v.ID, &v.DatabaseID, &v.Name, &v.Type, &rawFilters,
		&v.SortBy, &v.SortDir, &v.GroupBy, &hidden, &v.CreatedAt); err != nil {
		return nil, err
	}
	if len(rawFilters) > 0 {
		_ = json.Unmarshal(rawFilters, &v.Filters)
	}
	if v.Filters == nil {
		v.Filters = []Filter{}
	}
	v.HiddenCols = hidden
	if v.HiddenCols == nil {
		v.HiddenCols = []string{}
	}
	return &v, nil
}

func (s *Store) CreateView(ctx context.Context, v DatabaseView, wsIDs []string) (*DatabaseView, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	// A member of A can't add views to B's database.
	if err := s.assertDatabaseInWorkspaces(ctx, v.DatabaseID, wsIDs); err != nil {
		return nil, err
	}
	if v.Type == "" {
		v.Type = ViewTable
	}
	if v.SortDir == "" {
		v.SortDir = "asc"
	}
	if v.Name == "" {
		v.Name = strings.Title(string(v.Type))
	}
	if v.Filters == nil {
		v.Filters = []Filter{}
	}
	if v.HiddenCols == nil {
		v.HiddenCols = []string{}
	}
	filters, _ := json.Marshal(v.Filters)
	row := s.pool.QueryRow(ctx,
		`INSERT INTO database_views (database_id, name, type, filters, sort_by, sort_dir, group_by, hidden_cols)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        RETURNING `+viewSelectCols,
		v.DatabaseID, v.Name, string(v.Type), filters, v.SortBy, v.SortDir, v.GroupBy, v.HiddenCols,
	)
	return scanView(row)
}

func (s *Store) ListViews(ctx context.Context, databaseID string, wsIDs []string) ([]DatabaseView, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+viewSelectCols+` FROM database_views WHERE database_id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))
        ORDER BY created_at ASC`,
		databaseID, wsIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("database: list views: %w", err)
	}
	defer rows.Close()
	var out []DatabaseView
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// UpdateView accepts a partial map of fields to change. We
// allow-list the keys so callers can't smuggle in arbitrary SQL
// fragments via column names.
func (s *Store) UpdateView(ctx context.Context, id string, updates map[string]any, wsIDs []string) (*DatabaseView, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	allowed := map[string]bool{
		"name": true, "type": true, "filters": true,
		"sort_by": true, "sort_dir": true, "group_by": true, "hidden_cols": true,
	}
	var (
		setParts []string
		args     []any
	)
	idx := 1
	for k, v := range updates {
		if !allowed[k] {
			continue
		}
		// JSON-encode the filters slice; everything else passes through.
		if k == "filters" {
			b, _ := json.Marshal(v)
			args = append(args, b)
		} else {
			args = append(args, v)
		}
		setParts = append(setParts, fmt.Sprintf("%s = $%d", k, idx))
		idx++
	}
	if len(setParts) == 0 {
		return nil, errors.New("database: no updatable fields")
	}
	args = append(args, id)
	idPos := idx
	idx++
	args = append(args, wsIDs)
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`UPDATE database_views SET %s WHERE id = $%d
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($%d)) RETURNING %s`,
			strings.Join(setParts, ", "), idPos, idx, viewSelectCols),
		args...,
	)
	v, err := scanView(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}
