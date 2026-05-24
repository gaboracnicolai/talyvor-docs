// Package analytics owns the per-view event log + readership
// roll-ups. The pages.view_count counter remains the cheap path for
// "page info"; this package backs the richer Analytics screens
// (per-page line chart, workspace-wide most/least read).
package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// minDuration drops accidental clicks. 3 seconds matches the spec —
// shorter views frequently come from sidebar mis-clicks or
// browser-history back-button traffic.
const minDuration = 3

type PageView struct {
	ID          string    `json:"id"`
	PageID      string    `json:"page_id"`
	WorkspaceID string    `json:"workspace_id"`
	ViewerID    string    `json:"viewer_id"`
	ViewerName  string    `json:"viewer_name"`
	Duration    int       `json:"duration_sec"`
	CreatedAt   time.Time `json:"created_at"`
}

type DayCount struct {
	Date  time.Time `json:"date"`
	Count int       `json:"count"`
}

type ViewerStat struct {
	ViewerID   string    `json:"viewer_id"`
	ViewerName string    `json:"viewer_name"`
	ViewCount  int       `json:"view_count"`
	LastViewed time.Time `json:"last_viewed"`
}

type ReadStats struct {
	PageID         string       `json:"page_id"`
	Title          string       `json:"title"`
	TotalViews     int          `json:"total_views"`
	UniqueViewers  int          `json:"unique_viewers"`
	AvgDurationSec int          `json:"avg_duration_sec"`
	LastViewedAt   *time.Time   `json:"last_viewed_at,omitempty"`
	ViewsByDay     []DayCount   `json:"views_by_day"`
	TopViewers     []ViewerStat `json:"top_viewers"`
}

type WorkspaceReadStats struct {
	TotalViews     int         `json:"total_views"`
	UniqueViewers  int         `json:"unique_viewers"`
	MostReadPages  []ReadStats `json:"most_read_pages"`
	LeastReadPages []ReadStats `json:"least_read_pages"`
	NeverRead      int         `json:"never_read_count"`
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

// RecordView appends a row to page_views and bumps the cached
// counter on pages. Views under minDuration are dropped — accidental
// clicks shouldn't pollute analytics. Anonymous viewers come through
// as viewer_id="anonymous" so the schema's default is fine.
func (s *Store) RecordView(ctx context.Context, view PageView) error {
	if s.pool == nil {
		return errors.New("analytics: no pool")
	}
	if view.Duration < minDuration {
		return nil
	}
	if view.ViewerID == "" {
		view.ViewerID = "anonymous"
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO page_views (page_id, workspace_id, viewer_id, viewer_name, duration_sec)
        VALUES ($1, $2, $3, $4, $5)`,
		view.PageID, view.WorkspaceID, view.ViewerID, view.ViewerName, view.Duration,
	); err != nil {
		return fmt.Errorf("analytics: insert view: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE pages SET view_count = view_count + 1, last_viewed_at = NOW() WHERE id = $1`,
		view.PageID,
	); err != nil {
		return fmt.Errorf("analytics: bump counter: %w", err)
	}
	return nil
}

// GetReadStats returns the per-page roll-up over the past `days`
// days. Three queries (totals, day-buckets, top-viewers) keep each
// SQL simple; combining them with CTEs hurt readability without
// meaningful perf gain at our scale.
func (s *Store) GetReadStats(ctx context.Context, pageID string, days int) (*ReadStats, error) {
	if s.pool == nil {
		return nil, nil
	}
	if days <= 0 {
		days = 30
	}
	var out ReadStats
	out.PageID = pageID

	var lastViewed sql.NullTime
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*)::int, COUNT(DISTINCT viewer_id)::int,
                COALESCE(AVG(duration_sec)::int, 0),
                MAX(created_at)
        FROM page_views
        WHERE page_id = $1
          AND created_at > NOW() - INTERVAL '1 day' * $2`,
		pageID, days,
	).Scan(&out.TotalViews, &out.UniqueViewers, &out.AvgDurationSec, &lastViewed); err != nil {
		return nil, fmt.Errorf("analytics: totals: %w", err)
	}
	if lastViewed.Valid {
		t := lastViewed.Time
		out.LastViewedAt = &t
	}

	// Views per day.
	rows, err := s.pool.Query(ctx,
		`SELECT DATE_TRUNC('day', created_at), COUNT(*)::int
        FROM page_views
        WHERE page_id = $1
          AND created_at > NOW() - INTERVAL '1 day' * $2
        GROUP BY 1
        ORDER BY 1`,
		pageID, days,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics: by day: %w", err)
	}
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Date, &d.Count); err != nil {
			rows.Close()
			return nil, err
		}
		out.ViewsByDay = append(out.ViewsByDay, d)
	}
	rows.Close()

	// Top viewers (5).
	rows, err = s.pool.Query(ctx,
		`SELECT viewer_id, MAX(viewer_name), COUNT(*)::int, MAX(created_at)
        FROM page_views
        WHERE page_id = $1
          AND created_at > NOW() - INTERVAL '1 day' * $2
        GROUP BY viewer_id
        ORDER BY COUNT(*) DESC
        LIMIT 5`,
		pageID, days,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics: top viewers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v ViewerStat
		if err := rows.Scan(&v.ViewerID, &v.ViewerName, &v.ViewCount, &v.LastViewed); err != nil {
			return nil, err
		}
		out.TopViewers = append(out.TopViewers, v)
	}
	return &out, nil
}

// GetWorkspaceStats rolls up the same window across every page in
// the workspace. The "never read" count is computed against the
// pages table (not page_views) so pages that were created without
// any traffic show up.
func (s *Store) GetWorkspaceStats(ctx context.Context, workspaceID string, days int) (*WorkspaceReadStats, error) {
	if s.pool == nil {
		return nil, nil
	}
	if days <= 0 {
		days = 30
	}
	var out WorkspaceReadStats

	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*)::int, COUNT(DISTINCT viewer_id)::int
        FROM page_views
        WHERE workspace_id = $1
          AND created_at > NOW() - INTERVAL '1 day' * $2`,
		workspaceID, days,
	).Scan(&out.TotalViews, &out.UniqueViewers); err != nil {
		return nil, fmt.Errorf("analytics: workspace totals: %w", err)
	}

	// Most read (top 10).
	rows, err := s.pool.Query(ctx,
		`SELECT pv.page_id, MAX(p.title), COUNT(*)::int
        FROM page_views pv
        JOIN pages p ON p.id = pv.page_id
        WHERE pv.workspace_id = $1
          AND pv.created_at > NOW() - INTERVAL '1 day' * $2
        GROUP BY pv.page_id
        ORDER BY COUNT(*) DESC
        LIMIT 10`,
		workspaceID, days,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics: most read: %w", err)
	}
	for rows.Next() {
		var r ReadStats
		if err := rows.Scan(&r.PageID, &r.Title, &r.TotalViews); err != nil {
			rows.Close()
			return nil, err
		}
		out.MostReadPages = append(out.MostReadPages, r)
	}
	rows.Close()

	// Least read with > 0 views (bottom 10). Pages with zero views
	// flow into the NeverRead bucket below — splitting the cohorts
	// makes the UI's "Needs attention" + "Never read" sections
	// independently driven.
	rows, err = s.pool.Query(ctx,
		`SELECT pv.page_id, MAX(p.title), COUNT(*)::int
        FROM page_views pv
        JOIN pages p ON p.id = pv.page_id
        WHERE pv.workspace_id = $1
          AND pv.created_at > NOW() - INTERVAL '1 day' * $2
        GROUP BY pv.page_id
        ORDER BY COUNT(*) ASC
        LIMIT 10`,
		workspaceID, days,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics: least read: %w", err)
	}
	for rows.Next() {
		var r ReadStats
		if err := rows.Scan(&r.PageID, &r.Title, &r.TotalViews); err != nil {
			rows.Close()
			return nil, err
		}
		out.LeastReadPages = append(out.LeastReadPages, r)
	}
	rows.Close()

	// Never read — pages that have never received a single view.
	// Templates are excluded (they're boilerplate, not content).
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*)::int FROM pages p
        LEFT JOIN page_views pv ON pv.page_id = p.id
        WHERE p.workspace_id = $1 AND p.is_template = false
          AND pv.id IS NULL`,
		workspaceID,
	).Scan(&out.NeverRead); err != nil {
		return nil, fmt.Errorf("analytics: never read: %w", err)
	}

	return &out, nil
}
