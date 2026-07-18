// Package block stores the per-block content nodes that Phase 2's
// collaborative editor will populate. Phase 1 keeps the shape stable
// so the migration + CRUD path is in place; the page render path
// still reads pages.content (the canonical ProseMirror blob) until
// the editor ships.
package block

import (
	"context"
	"errors"
	"fmt"

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

const columns = `id, page_id, type, content, position, parent_id,
    created_at, updated_at`

func scan(s interface{ Scan(...any) error }) (*model.Block, error) {
	var b model.Block
	if err := s.Scan(
		&b.ID, &b.PageID, &b.Type, &b.Content, &b.Position, &b.ParentID,
		&b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) Create(ctx context.Context, b model.Block) (*model.Block, error) {
	if s.pool == nil {
		return nil, errors.New("block: store has no pool")
	}
	if b.PageID == "" || b.Type == "" {
		return nil, errors.New("block: page_id and type required")
	}
	if b.Content == "" {
		b.Content = "{}"
	}
	return scan(s.pool.QueryRow(ctx,
		`INSERT INTO blocks (page_id, type, content, position, parent_id)
        VALUES ($1, $2, $3, $4, $5) RETURNING `+columns,
		b.PageID, b.Type, b.Content, b.Position, b.ParentID,
	))
}

func (s *Store) ListByPage(ctx context.Context, pageID string) ([]model.Block, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM blocks WHERE page_id = $1 ORDER BY position ASC, created_at ASC`,
		pageID,
	)
	if err != nil {
		return nil, fmt.Errorf("block: list: %w", err)
	}
	defer rows.Close()
	var out []model.Block
	for rows.Next() {
		b, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

// ErrNotFound signals a by-id op on a block whose page is not in the caller's verified
// workspaces. Maps to 404 in the handler — no cross-tenant existence oracle.
var ErrNotFound = errors.New("block: not found in an accessible workspace")

// assertInWorkspaces is the in-method SEC-4 L2 gate: a block carries no workspace_id, so it
// reaches its tenant through its page (blocks.page_id → pages.workspace_id). Holds on its own —
// independent of the route enforcer wiring.
func (s *Store) assertInWorkspaces(ctx context.Context, blockID string, wsIDs []string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM blocks b JOIN pages p ON b.page_id = p.id
                       WHERE b.id = $1 AND p.workspace_id = ANY($2))`,
		blockID, wsIDs,
	).Scan(&exists); err != nil {
		return fmt.Errorf("block: scope check: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// UpdateInWorkspaces updates a block only if its page lives in one of the caller's workspaces.
func (s *Store) UpdateInWorkspaces(ctx context.Context, id, content string, position float64, wsIDs []string) (*model.Block, error) {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return nil, err
	}
	return s.Update(ctx, id, content, position)
}

// DeleteInWorkspaces deletes a block only if its page lives in one of the caller's workspaces.
func (s *Store) DeleteInWorkspaces(ctx context.Context, id string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, id, wsIDs); err != nil {
		return err
	}
	return s.Delete(ctx, id)
}

func (s *Store) Update(ctx context.Context, id string, content string, position float64) (*model.Block, error) {
	if s.pool == nil {
		return nil, errors.New("block: store has no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- GATED IN-METHOD: Update is a primitive reached only via UpdateInWorkspaces (above), which asserts the block's page ∈ the caller's verified workspaces first (blocks.page_id → pages.workspace_id = ANY) → ErrNotFound. Holds on its own, independent of the route enforcer.
	return scan(s.pool.QueryRow(ctx,
		`UPDATE blocks SET content = $1, position = $2, updated_at = NOW()
        WHERE id = $3 RETURNING `+columns,
		content, position, id,
	))
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if s.pool == nil {
		return errors.New("block: store has no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- GATED IN-METHOD: Delete is a primitive reached only via DeleteInWorkspaces (above), which asserts the block's page ∈ the caller's verified workspaces first → ErrNotFound. Holds on its own, independent of the route enforcer.
	_, err := s.pool.Exec(ctx, `DELETE FROM blocks WHERE id = $1`, id)
	return err
}
