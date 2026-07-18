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

func (s *Store) Update(ctx context.Context, id string, content string, position float64) (*model.Block, error) {
	if s.pool == nil {
		return nil, errors.New("block: store has no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- EXTERNALLY GATED (blocks carry page_id, not workspace_id): the sole caller is handler.go Update, whose route PATCH /blocks/{blockID} is wrapped in blockEnf.Require. blockEnf resolves via blockPageLooker (cmd/docs/main.go) → the block's page → GetByIDInWorkspaces, so a workspace-B block resolves to a workspace-B page → ErrNotFound → 404. Same gate as block.Delete below.
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
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- EXTERNALLY GATED (blocks carry page_id, not workspace_id): the sole caller is handler.go Delete, whose route is wrapped in blockEnf.Require in this package's Mount. blockEnf resolves via blockPageLooker (cmd/docs/main.go): its first hop `SELECT page_id FROM blocks WHERE id=$1` is unscoped, but it feeds that page id straight to pageLooker → GetByIDInWorkspaces, so a workspace-B block resolves to a workspace-B page → ErrNotFound → 404. NOTE: the gate is main.go's WithAccess wiring, NOT this file (Enforcer.Require is pass-through on a nil receiver).
	_, err := s.pool.Exec(ctx, `DELETE FROM blocks WHERE id = $1`, id)
	return err
}
