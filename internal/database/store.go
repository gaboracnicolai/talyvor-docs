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

// MaxColumns + MaxRows are the spec constraints. ⚠ ONLY ONE OF THEM IS ENFORCED, AND THIS COMMENT
// USED TO CLAIM BOTH WERE: it read "Bounded so a runaway user (or agent) can't blow up a page
// render", which is true of MaxColumns and false of MaxRows.
//
//	MaxColumns IS enforced — validateSchema, reached from Create and UpdateSchema (below).
//	MaxRows is NOT enforced anywhere. CreateRow performs no count check; the constant's only
//	other mention in the tree is ListRows' comment, which CITES it as the reason that read is
//	safe to leave unpaginated. MEASURED on real Postgres through the shipped store
//	(maxrows_realpg_test.go): CreateRow accepts row 10_001 without error and ListRows then
//	returns all 10_001 in one fetch.
//
// So MaxRows currently documents an intention, not a behaviour. Whether to make it real is a
// PRODUCT call and not a plumbing one — enforcing it decides what happens to a customer who
// already holds more than 10_000 rows — which is why this comment was corrected rather than the
// code. Do not "fix" this by adding the check without answering that question first.
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
//
// databaseID IS THE DATABASE THE ROUTE'S ENFORCER AUTHORIZED, and both statements below are
// scoped to it. The route is `.With(dbEnf.Require(AccessEdit))` on {dbID}, so the gate answers
// about THAT database; passing only the row id left the gate and the statement talking about two
// different objects, with the workspace ring — which both are already inside — the only thing
// between them. A member with a page of their own could then rewrite, and read back, any row in
// any database in the workspace, including one on a private page they cannot open.
//
// BOTH STATEMENTS CARRY THE PREDICATE, AND NEITHER IS INDIVIDUALLY NECESSARY — measured, not
// assumed. This is a read-modify-write that RETURNS the merged row, so the disclosure and the
// tamper travel together: the merged row only ever reaches the caller through the UPDATE's
// RETURNING, so scoping EITHER statement alone already answers 404 and lets nothing out
// (scripts/w31-database-crossdb-controls.py C2 and C3 are both silent at the product level; C4,
// which removes both, is what earns this). They are both scoped anyway, because the shape that
// makes one sufficient is a fact about today's code — replace the Go-side merge with a JSONB `||`
// and the SELECT disappears; move the read behind a helper and the UPDATE's scope is all that is
// left. Each statement stating its own object costs nothing and survives either refactor.
func (s *Store) UpdateRow(ctx context.Context, databaseID, id string, patch map[string]any, wsIDs []string) (*Row, error) {
	if s.pool == nil {
		return nil, errors.New("database: no pool")
	}
	if patch == nil {
		patch = map[string]any{}
	}
	var existing []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT values FROM database_rows WHERE id = $1 AND database_id = $2
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($3))`, id, databaseID, wsIDs,
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
		`UPDATE database_rows SET values = $1, updated_at = NOW() WHERE id = $2 AND database_id = $3
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($4)) RETURNING `+rowCols,
		encoded, id, databaseID, wsIDs,
	)
	r, err := scanRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// DeleteRow removes one row of databaseID — the database the route's enforcer authorized. See
// UpdateRow for why the database id is a parameter and not just the workspace set.
func (s *Store) DeleteRow(ctx context.Context, databaseID, id string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("database: no pool")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM database_rows WHERE id = $1 AND database_id = $2
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($3))`, id, databaseID, wsIDs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListRows fetches every row for the database, then applies the
// view's filters + sort in Go, rather than building dynamic WHERE
// clauses against JSONB — which keeps the rule engine unit-testable.
//
// ⚠ THE OTHER HALF OF THAT JUSTIFICATION WAS FALSE AND IS DELETED. It read "because the row counts
// are bounded (MaxRows = 10K)". Nothing bounds them: MaxRows is declared and enforced at no site,
// CreateRow has no count check, and MEASURED through this very method on real Postgres
// (maxrows_realpg_test.go) a database of 10_001 rows comes back whole — all 10_001 into memory, in
// one query, on a shipped route. The fetch-everything shape is therefore a deliberate trade with
// an OPEN upper bound, not a bounded one; see the note on the const block for why closing it is a
// product decision rather than a missing `if`.
//
// ⚠ `ORDER BY position ASC, created_at ASC, id ASC` — THE TIEBREAK IS NOT DECORATION, AND TIES ARE
// THE ORDINARY CASE HERE. `position` is a client-supplied FLOAT and the SPA computes it as
// `rows.length + 1` (DatabaseBlock.tsx:89), so deleting any row and adding another hands the new
// row the position an existing one already holds. `position ASC` alone then left the answer to the
// heap: measured on real Postgres, delete-the-middle + "+ New" returned the NEW row ABOVE the older
// one it tied with, because it reused the freed slot once the space was reclaimed. `page/store.go`
// states the rule this obeys — an ORDER BY that names no unique column has NO defined relative
// order — and `id` is the PRIMARY KEY, which is what makes this one total: `created_at` is `NOW()`,
// so rows written in a single transaction share it. `created_at` sits ahead of `id` so ties read as
// creation order rather than as UUID order, `id` being `gen_random_uuid()`.
//
// This ordering also feeds `sortRows` below, which is a `sort.SliceStable` — a stable sort
// PRESERVES its input order for equal keys, so an undefined order here stayed undefined all the way
// to the client rather than being re-decided. Pinned by
// TestListRows_TiedPositionsHaveADefinedOrder_RealPG, which asserts both halves.
func (s *Store) ListRows(ctx context.Context, databaseID string, view *DatabaseView, wsIDs []string) ([]Row, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+rowCols+` FROM database_rows WHERE database_id = $1
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($2))
        ORDER BY position ASC, created_at ASC, id ASC`,
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
//
// databaseID IS THE DATABASE THE ROUTE'S ENFORCER AUTHORIZED — see UpdateRow. The statement
// RETURNS the merged view, so this is a disclosure as well as a tamper: group_by, sort_by and
// hidden_cols name COLUMNS of a schema the caller may have no right to read.
func (s *Store) UpdateView(ctx context.Context, databaseID, id string, updates map[string]any, wsIDs []string) (*DatabaseView, error) {
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
	args = append(args, databaseID)
	dbPos := idx
	idx++
	args = append(args, wsIDs)
	row := s.pool.QueryRow(ctx,
		fmt.Sprintf(`UPDATE database_views SET %s WHERE id = $%d AND database_id = $%d
        AND database_id IN (SELECT id FROM databases WHERE workspace_id = ANY($%d)) RETURNING %s`,
			strings.Join(setParts, ", "), idPos, dbPos, idx, viewSelectCols),
		args...,
	)
	v, err := scanView(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}
