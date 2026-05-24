// Package pagelink tracks which Track issues are referenced from
// which Docs pages. Two surfaces use it:
//
//   - When a page's content changes, SyncLinks scans the
//     ProseMirror JSON for issue_embed nodes and reconciles the
//     page_links table (additions + removals in one pass).
//   - The page handler's right-panel "Linked issues" section reads
//     ListByPage to render backlinks.
package pagelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PageLink struct {
	ID          string    `json:"id"`
	PageID      string    `json:"page_id"`
	WorkspaceID string    `json:"workspace_id"`
	IssueID     string    `json:"issue_id"`
	LinkType    string    `json:"link_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
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

const columns = `id, page_id, workspace_id, issue_id, link_type, created_by, created_at`

func scan(s interface{ Scan(...any) error }) (*PageLink, error) {
	var l PageLink
	if err := s.Scan(
		&l.ID, &l.PageID, &l.WorkspaceID, &l.IssueID, &l.LinkType, &l.CreatedBy, &l.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &l, nil
}

// Upsert inserts the link if it doesn't already exist. UNIQUE
// (page_id, issue_id, link_type) backs the dedupe — a no-op insert
// returns 0 rows affected and isn't treated as an error.
func (s *Store) Upsert(ctx context.Context, l PageLink) error {
	if s.pool == nil {
		return errors.New("pagelink: store has no pool")
	}
	if l.PageID == "" || l.IssueID == "" {
		return errors.New("pagelink: page_id and issue_id required")
	}
	if l.LinkType == "" {
		l.LinkType = "embed"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO page_links (page_id, workspace_id, issue_id, link_type, created_by)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (page_id, issue_id, link_type) DO NOTHING`,
		l.PageID, l.WorkspaceID, l.IssueID, l.LinkType, l.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("pagelink: upsert: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, pageID, issueID string) error {
	if s.pool == nil {
		return errors.New("pagelink: store has no pool")
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM page_links WHERE page_id = $1 AND issue_id = $2`,
		pageID, issueID,
	)
	return err
}

func (s *Store) ListByPage(ctx context.Context, pageID string) ([]PageLink, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM page_links WHERE page_id = $1 ORDER BY created_at ASC`,
		pageID,
	)
	if err != nil {
		return nil, fmt.Errorf("pagelink: list by page: %w", err)
	}
	defer rows.Close()
	var out []PageLink
	for rows.Next() {
		l, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// IssueIDsForPage is the cost-syncer's narrow read — only embed-
// typed links count toward a page's AI-cost roll-up so manual
// "mention" / "spec" annotations don't double-count.
func (s *Store) IssueIDsForPage(ctx context.Context, pageID string) ([]string, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT issue_id FROM page_links WHERE page_id = $1 AND link_type = 'embed'`,
		pageID,
	)
	if err != nil {
		return nil, fmt.Errorf("pagelink: issue ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) ListByIssue(ctx context.Context, issueID string) ([]PageLink, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM page_links WHERE issue_id = $1 ORDER BY created_at ASC`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("pagelink: list by issue: %w", err)
	}
	defer rows.Close()
	var out []PageLink
	for rows.Next() {
		l, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *l)
	}
	return out, rows.Err()
}

// ─── ProseMirror scanner ──────────────────────────────────

// ParseEmbeds walks a ProseMirror JSON doc and returns every
// issue_id attribute on issue_embed nodes. Malformed JSON returns
// an empty slice — embed extraction is best-effort and must never
// break the save path. Duplicates are de-duped so re-emitting the
// same embed in two paragraphs only counts once.
func ParseEmbeds(prosemirror string) []string {
	if prosemirror == "" || prosemirror == "{}" {
		return nil
	}
	var doc any
	if err := json.Unmarshal([]byte(prosemirror), &doc); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	walk(doc, func(node map[string]any) {
		if node["type"] == "issue_embed" {
			attrs, _ := node["attrs"].(map[string]any)
			id, _ := attrs["issue_id"].(string)
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	})
	return out
}

func walk(node any, visit func(map[string]any)) {
	switch v := node.(type) {
	case map[string]any:
		visit(v)
		if content, ok := v["content"]; ok {
			walk(content, visit)
		}
	case []any:
		for _, child := range v {
			walk(child, visit)
		}
	}
}

// ─── SyncLinks (diff-based) ───────────────────────────────

// SyncLinks reconciles the embed links for a page with the embeds
// actually present in its new content. It reads the current set,
// computes added + removed, and applies them. Best-effort: a
// transient failure mid-batch leaves the table partially synced —
// the next save reconciles again.
func (s *Store) SyncLinks(ctx context.Context, pageID, workspaceID, content, createdBy string) error {
	if s.pool == nil {
		return nil
	}
	// Existing embed-typed links.
	rows, err := s.pool.Query(ctx,
		`SELECT issue_id FROM page_links WHERE page_id = $1 AND link_type = $2`,
		pageID, "embed",
	)
	if err != nil {
		return fmt.Errorf("pagelink: sync read: %w", err)
	}
	existing := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		existing[id] = true
	}
	rows.Close()

	current := map[string]bool{}
	for _, id := range ParseEmbeds(content) {
		current[id] = true
	}

	// Add the new entries.
	for id := range current {
		if existing[id] {
			continue
		}
		if err := s.Upsert(ctx, PageLink{
			PageID:      pageID,
			WorkspaceID: workspaceID,
			IssueID:     id,
			LinkType:    "embed",
			CreatedBy:   createdBy,
		}); err != nil {
			return err
		}
	}
	// Remove embeds the user deleted from the content. We only touch
	// embed-typed links here so "mention" / "spec" rows added via
	// the UI survive a content edit.
	for id := range existing {
		if current[id] {
			continue
		}
		if err := s.Delete(ctx, pageID, id); err != nil {
			return err
		}
	}
	return nil
}
