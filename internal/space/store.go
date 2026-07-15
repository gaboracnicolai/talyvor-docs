// Package space owns the database operations for documentation
// spaces. Pages nest under spaces; the workspace_id field is an
// opaque tenant key that ties back to Talyvor Track (no FK — Docs
// can run standalone).
package space

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/model"
)

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

// Slugify maps a free-form name to a URL-safe slug. Exported so
// callers (handler, importer) can pre-validate before hitting the
// store. Non-alphanumeric runs collapse to single hyphens.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevHyphen := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevHyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !prevHyphen {
				b.WriteRune('-')
				prevHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

const columns = `id, workspace_id, name, slug, description,
    icon, color, private, created_by,
    created_at, updated_at`

func scan(s interface{ Scan(...any) error }) (*model.Space, error) {
	var sp model.Space
	if err := s.Scan(
		&sp.ID, &sp.WorkspaceID, &sp.Name, &sp.Slug, &sp.Description,
		&sp.Icon, &sp.Color, &sp.Private, &sp.CreatedBy,
		&sp.CreatedAt, &sp.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &sp, nil
}

func (s *Store) Create(ctx context.Context, sp model.Space) (*model.Space, error) {
	if s.pool == nil {
		return nil, errors.New("space: store has no pool")
	}
	if strings.TrimSpace(sp.Name) == "" {
		return nil, errors.New("space: name required")
	}
	if sp.Slug == "" {
		sp.Slug = Slugify(sp.Name)
	}
	if !slugRe.MatchString(sp.Slug) {
		return nil, fmt.Errorf("space: invalid slug %q", sp.Slug)
	}
	if sp.Icon == "" {
		sp.Icon = "📄"
	}
	if sp.Color == "" {
		sp.Color = "#6366f1"
	}
	return scan(s.pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, description, icon, color, private, created_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+columns,
		sp.WorkspaceID, strings.TrimSpace(sp.Name), sp.Slug, sp.Description,
		sp.Icon, sp.Color, sp.Private, sp.CreatedBy,
	))
}

func (s *Store) GetByID(ctx context.Context, id string) (*model.Space, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM spaces WHERE id = $1`, id))
}

func (s *Store) GetBySlug(ctx context.Context, workspaceID, slug string) (*model.Space, error) {
	return scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM spaces WHERE workspace_id = $1 AND slug = $2`,
		workspaceID, slug))
}

func (s *Store) List(ctx context.Context, workspaceID string) ([]model.Space, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM spaces WHERE workspace_id = $1
        ORDER BY name ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("space: list: %w", err)
	}
	defer rows.Close()
	var out []model.Space
	for rows.Next() {
		sp, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sp)
	}
	return out, rows.Err()
}

// updatable is the allowlist of columns Update will touch. Hard-coding
// the set keeps an injection-via-map-key attack impossible.
var updatable = map[string]struct{}{
	"name": {}, "description": {}, "icon": {}, "color": {}, "private": {},
}

func (s *Store) Update(ctx context.Context, id string, updates map[string]any) (*model.Space, error) {
	if s.pool == nil {
		return nil, errors.New("space: store has no pool")
	}
	if len(updates) == 0 {
		return s.GetByID(ctx, id)
	}
	var (
		set  []string
		args []any
		n    int
	)
	for k, v := range updates {
		if _, ok := updatable[k]; !ok {
			continue
		}
		n++
		set = append(set, fmt.Sprintf("%s = $%d", k, n))
		args = append(args, v)
	}
	if len(set) == 0 {
		return s.GetByID(ctx, id)
	}
	n++
	set = append(set, fmt.Sprintf("updated_at = $%d", n))
	args = append(args, time.Now().UTC())
	n++
	args = append(args, id)

	//nosemgrep: docs-by-id-write-requires-workspace-scope-sprintf -- Update is a primitive reached only via UpdateInWorkspaces, which calls assertInWorkspaces(id, authz.WorkspaceIDs) first (404s a foreign space before this by-id write).
	sql := fmt.Sprintf(`UPDATE spaces SET %s WHERE id = $%d RETURNING %s`,
		strings.Join(set, ", "), n, columns)
	return scan(s.pool.QueryRow(ctx, sql, args...))
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if s.pool == nil {
		return errors.New("space: store has no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- Delete is a primitive reached only via DeleteInWorkspaces (store.go), which calls assertInWorkspaces(id, authz.WorkspaceIDs) first (404s a foreign space before this by-id write). Same pattern as Update above.
	_, err := s.pool.Exec(ctx, `DELETE FROM spaces WHERE id = $1`, id)
	return err
}

// ─── SEC-4 Layer 2: workspace-scoped by-id ops ─────────────
//
// The authed /v1 space handler uses these; a space in a workspace the caller doesn't belong
// to is invisible → ErrNotFound → 404. wsIDs comes from verified membership (Layer 1).

// ErrNotFound signals a by-id op resolved to no space in the caller's workspaces.
var ErrNotFound = errors.New("space: not found in an accessible workspace")

func (s *Store) assertInWorkspaces(ctx context.Context, id string, wsIDs []string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM spaces WHERE id = $1 AND workspace_id = ANY($2))`,
		id, wsIDs,
	).Scan(&exists); err != nil {
		return fmt.Errorf("space: scope check: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// GetByIDInWorkspaces reads a space only if it lives in one of the caller's workspaces.
func (s *Store) GetByIDInWorkspaces(ctx context.Context, id string, wsIDs []string) (*model.Space, error) {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return nil, err
	}
	return s.GetByID(ctx, id)
}

// UpdateInWorkspaces mutates a space only if it lives in one of the caller's workspaces.
func (s *Store) UpdateInWorkspaces(ctx context.Context, id string, updates map[string]any, wsIDs []string) (*model.Space, error) {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return nil, err
	}
	return s.Update(ctx, id, updates)
}

// DeleteInWorkspaces deletes a space only if it lives in one of the caller's workspaces.
func (s *Store) DeleteInWorkspaces(ctx context.Context, id string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return err
	}
	return s.Delete(ctx, id)
}
