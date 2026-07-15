// Package comment is the threaded-comment surface. One row per
// utterance in page_comments; thread_id groups a top-level comment
// with its replies. Resolve / Unresolve operate on the whole
// thread so the UX is "this conversation is settled" rather than
// "this specific message".
package comment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Comment struct {
	ID         string     `json:"id"`
	PageID     string     `json:"page_id"`
	BlockID    *string    `json:"block_id,omitempty"`
	ThreadID   *string    `json:"thread_id,omitempty"`
	ParentID   *string    `json:"parent_id,omitempty"`
	AuthorID   string     `json:"author_id"`
	AuthorName string     `json:"author_name"`
	Content    string     `json:"content"`
	Resolved   bool       `json:"resolved"`
	ResolvedBy *string    `json:"resolved_by,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	Replies    []Comment  `json:"replies,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type CommentStats struct {
	Total    int `json:"total"`
	Open     int `json:"open"`
	Resolved int `json:"resolved"`
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

const cols = `id, page_id, block_id, thread_id, parent_id, author_id, author_name, content, resolved, resolved_by, resolved_at, created_at, updated_at`

func scan(s interface{ Scan(...any) error }) (*Comment, error) {
	var c Comment
	if err := s.Scan(
		&c.ID, &c.PageID, &c.BlockID, &c.ThreadID, &c.ParentID,
		&c.AuthorID, &c.AuthorName, &c.Content,
		&c.Resolved, &c.ResolvedBy, &c.ResolvedAt,
		&c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &c, nil
}

// ─── Create / Reply ─────────────────────────────────

// Create inserts a top-level comment whose thread_id equals its own id (a top-level
// comment is its own thread root). The id is generated up front in a CTE and used for
// BOTH id and thread_id in a single atomic INSERT ... SELECT. This replaces an earlier
// `WITH inserted AS (INSERT ...) UPDATE page_comments ... FROM inserted` form that was
// doubly broken on real Postgres — an ambiguous `id` in RETURNING (42702), and, even once
// disambiguated, the UPDATE could not see the CTE-inserted row (same snapshot) so it
// matched 0 rows. pgxmock never executed the SQL, so both faults were invisible until the
// real-PG harness ran it.
func (s *Store) Create(ctx context.Context, pageID string, blockID *string, authorID, authorName, content string) (*Comment, error) {
	if s.pool == nil {
		return nil, errors.New("comment: no pool")
	}
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("comment: content required")
	}
	if authorID == "" {
		return nil, errors.New("comment: author_id required")
	}
	row := s.pool.QueryRow(ctx,
		`WITH new_id AS (SELECT gen_random_uuid()::text AS id)
        INSERT INTO page_comments (id, page_id, block_id, thread_id, author_id, author_name, content)
        SELECT new_id.id, $1, $2, new_id.id, $3, $4, $5
        FROM new_id
        RETURNING `+cols,
		pageID, blockID, authorID, authorName, content,
	)
	return scan(row)
}

// Reply attaches a comment under an existing parent. Inherits
// thread_id + page_id from the parent so listing logic stays
// uniform.
func (s *Store) Reply(ctx context.Context, parentID, authorID, authorName, content string) (*Comment, error) {
	if s.pool == nil {
		return nil, errors.New("comment: no pool")
	}
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("comment: content required")
	}
	var (
		threadID *string
		pageID   string
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT thread_id, page_id FROM page_comments WHERE id = $1`, parentID,
	).Scan(&threadID, &pageID); err != nil {
		return nil, fmt.Errorf("comment: parent not found: %w", err)
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO page_comments
        (page_id, block_id, thread_id, parent_id, author_id, author_name, content)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        RETURNING `+cols,
		pageID, (*string)(nil), threadID, &parentID, authorID, authorName, content,
	)
	return scan(row)
}

// ─── Resolve / Unresolve ────────────────────────────

// Resolve marks every comment in the same thread as resolved with
// the same timestamp + resolver. We resolve threads, not individual
// utterances — "this conversation is settled" is the model.
func (s *Store) Resolve(ctx context.Context, commentID, resolvedBy string) error {
	if s.pool == nil {
		return errors.New("comment: no pool")
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE page_comments
        SET resolved = true, resolved_by = $1, resolved_at = NOW(), updated_at = NOW()
        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $2)`,
		resolvedBy, commentID,
	)
	return err
}

func (s *Store) Unresolve(ctx context.Context, commentID string) error {
	if s.pool == nil {
		return errors.New("comment: no pool")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- Unresolve is a primitive reached only via UnresolveInWorkspaces (store.go), which calls assertInWorkspaces first (join on p.workspace_id = ANY($2)).
	_, err := s.pool.Exec(ctx,
		`UPDATE page_comments
        SET resolved = false, resolved_by = NULL, resolved_at = NULL, updated_at = NOW()
        WHERE thread_id = (SELECT thread_id FROM page_comments WHERE id = $1)`,
		commentID,
	)
	return err
}

// ─── ListByPage ──────────────────────────────────────

// ListByPage returns top-level comments with their replies nested.
// Walks every row for the page in (thread, created_at) order then
// buckets in Go — easier to read than a recursive CTE and fast for
// the row counts a single page generates.
func (s *Store) ListByPage(ctx context.Context, pageID string, includeResolved bool) ([]Comment, error) {
	if s.pool == nil {
		return nil, nil
	}
	q := `SELECT ` + cols + ` FROM page_comments WHERE page_id = $1`
	if !includeResolved {
		q += ` AND resolved = false`
	}
	q += ` ORDER BY thread_id ASC, created_at ASC`
	rows, err := s.pool.Query(ctx, q, pageID)
	if err != nil {
		return nil, fmt.Errorf("comment: list: %w", err)
	}
	defer rows.Close()
	threads := map[string]*Comment{}
	var heads []*Comment
	for rows.Next() {
		c, err := scan(rows)
		if err != nil {
			return nil, err
		}
		if c.ParentID == nil {
			threads[c.ID] = c
			heads = append(heads, c)
		} else {
			parent, ok := threads[*c.ParentID]
			if ok {
				parent.Replies = append(parent.Replies, *c)
			} else {
				// Orphaned reply (parent missing in this query —
				// could be resolved + filtered out). Surface it as
				// a top-level thread so the user can still see it.
				heads = append(heads, c)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(heads))
	for _, h := range heads {
		out = append(out, *h)
	}
	return out, nil
}

// ─── GetStats ────────────────────────────────────────

// GetStats returns the top-level thread counts for a page. Counts
// threads, not replies — "3 open" means 3 active conversations.
func (s *Store) GetStats(ctx context.Context, pageID string) (*CommentStats, error) {
	if s.pool == nil {
		return &CommentStats{}, nil
	}
	var total, resolved int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FILTER (WHERE parent_id IS NULL),
                COUNT(*) FILTER (WHERE parent_id IS NULL AND resolved = true)
        FROM page_comments WHERE page_id = $1`,
		pageID,
	).Scan(&total, &resolved); err != nil {
		return nil, fmt.Errorf("comment: stats: %w", err)
	}
	return &CommentStats{
		Total:    total,
		Resolved: resolved,
		Open:     total - resolved,
	}, nil
}

// ─── Delete ──────────────────────────────────────────

// Delete removes a comment only if requester is the author.
// ON DELETE CASCADE in the migration takes care of replies.
func (s *Store) Delete(ctx context.Context, commentID, requesterID string) error {
	if s.pool == nil {
		return errors.New("comment: no pool")
	}
	var author string
	if err := s.pool.QueryRow(ctx,
		`SELECT author_id FROM page_comments WHERE id = $1`, commentID,
	).Scan(&author); err != nil {
		return fmt.Errorf("comment: not found: %w", err)
	}
	if author != requesterID {
		return errors.New("comment: only the author can delete")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- Delete is a primitive reached only via DeleteInWorkspaces (store.go), which calls assertInWorkspaces first (join on p.workspace_id = ANY($2)); the author check above is the second gate.
	_, err := s.pool.Exec(ctx, `DELETE FROM page_comments WHERE id = $1`, commentID)
	return err
}

// ─── SEC-4 Layer 2: workspace-scoped by-id ops ─────────────
//
// page_comments has no workspace_id — a comment is scoped via its page (FK page_id → pages).
// The authed /v1 comment handler uses these variants; a comment whose page is in a workspace
// the caller doesn't belong to is invisible → ErrNotFound → 404. wsIDs comes from verified
// membership (Layer 1), never a client header.

// ErrNotFound signals a by-id op resolved to no comment on a page in the caller's workspaces.
var ErrNotFound = errors.New("comment: not found in an accessible workspace")

// assertInWorkspaces returns ErrNotFound unless commentID's page lives in one of wsIDs.
func (s *Store) assertInWorkspaces(ctx context.Context, commentID string, wsIDs []string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(
             SELECT 1 FROM page_comments c JOIN pages p ON c.page_id = p.id
             WHERE c.id = $1 AND p.workspace_id = ANY($2))`,
		commentID, wsIDs,
	).Scan(&exists); err != nil {
		return fmt.Errorf("comment: scope check: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

// ReplyInWorkspaces replies only if the parent comment's page is in one of the caller's workspaces.
func (s *Store) ReplyInWorkspaces(ctx context.Context, parentID, authorID, authorName, content string, wsIDs []string) (*Comment, error) {
	if err := s.assertInWorkspaces(ctx, parentID, wsIDs); err != nil {
		return nil, err
	}
	return s.Reply(ctx, parentID, authorID, authorName, content)
}

// ResolveInWorkspaces resolves only if the comment's page is in one of the caller's workspaces.
func (s *Store) ResolveInWorkspaces(ctx context.Context, commentID, resolvedBy string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, commentID, wsIDs); err != nil {
		return err
	}
	return s.Resolve(ctx, commentID, resolvedBy)
}

// UnresolveInWorkspaces unresolves only if the comment's page is in one of the caller's workspaces.
func (s *Store) UnresolveInWorkspaces(ctx context.Context, commentID string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, commentID, wsIDs); err != nil {
		return err
	}
	return s.Unresolve(ctx, commentID)
}

// DeleteInWorkspaces deletes only if the comment's page is in one of the caller's workspaces
// (in addition to the author-only check inside Delete).
func (s *Store) DeleteInWorkspaces(ctx context.Context, commentID, requesterID string, wsIDs []string) error {
	if err := s.assertInWorkspaces(ctx, commentID, wsIDs); err != nil {
		return err
	}
	return s.Delete(ctx, commentID, requesterID)
}
