// Package pin keeps a member's pinned pages on the server, and reads back the pages they opened
// most recently, so both follow the member between browsers and devices (B18.41).
package pin

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Entry is one page in a member's pinned or recent list. At is when it was pinned, or last opened.
type Entry struct {
	PageID  string    `json:"page_id"`
	SpaceID string    `json:"space_id"`
	Title   string    `json:"title"`
	At      time.Time `json:"at"`
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type Store struct{ pool pgxDB }

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Store{pool: db}
}

// maxPins bounds the list read; nobody's sidebar holds more.
const maxPins = 100

// Pin records that memberID pinned pageID in wsID. Pinning twice is not an error.
func (s *Store) Pin(ctx context.Context, wsID, memberID, pageID string) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO page_pins (workspace_id, member_id, page_id) VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, member_id, page_id) DO NOTHING`, wsID, memberID, pageID); err != nil {
		return fmt.Errorf("pin: insert: %w", err)
	}
	return nil
}

// Unpin removes the pin. Unpinning a page that is not pinned is not an error.
func (s *Store) Unpin(ctx context.Context, wsID, memberID, pageID string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM page_pins WHERE workspace_id = $1 AND member_id = $2 AND page_id = $3`,
		wsID, memberID, pageID); err != nil {
		return fmt.Errorf("pin: delete: %w", err)
	}
	return nil
}

// Pins lists memberID's pins in wsID, newest first.
func (s *Store) Pins(ctx context.Context, wsID, memberID string) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.space_id, p.title, pp.pinned_at
		FROM page_pins pp
		JOIN pages p ON p.id = pp.page_id AND p.workspace_id = pp.workspace_id
		WHERE pp.workspace_id = $1 AND pp.member_id = $2
		ORDER BY pp.pinned_at DESC
		LIMIT $3`, wsID, memberID, maxPins)
	return scanEntries(rows, err)
}

// Recent lists the n pages memberID opened most recently in wsID, from the page views Docs records
// for the verified viewer, newest first.
func (s *Store) Recent(ctx context.Context, wsID, memberID string, n int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.space_id, p.title, v.viewed_at
		FROM (
			SELECT page_id, MAX(created_at) AS viewed_at
			FROM page_views
			WHERE workspace_id = $1 AND viewer_id = $2
			GROUP BY page_id
			ORDER BY viewed_at DESC
			LIMIT $3
		) v
		JOIN pages p ON p.id = v.page_id AND p.workspace_id = $1
		ORDER BY v.viewed_at DESC`, wsID, memberID, n)
	return scanEntries(rows, err)
}

func scanEntries(rows pgx.Rows, err error) ([]Entry, error) {
	if err != nil {
		return nil, fmt.Errorf("pin: list: %w", err)
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.PageID, &e.SpaceID, &e.Title, &e.At); err != nil {
			return nil, fmt.Errorf("pin: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
