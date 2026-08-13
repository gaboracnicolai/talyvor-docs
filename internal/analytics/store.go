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

// pageVisibility is the per-page read gate the WORKSPACE ROLL-UP needs. It is the same interface
// freshness.FreshnessEngine takes, satisfied by the same *spaceauth.Authorizer main.go wires
// everywhere else — deliberately not a second copy of the permission rule expressed in SQL, which
// is how the engine and a hand-written predicate drift apart.
type pageVisibility interface {
	AuthorizePageRead(ctx context.Context, pageID string) (found, canView bool)
}

type Store struct {
	pool   pgxDB
	access pageVisibility
}

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(db pgxDB) *Store { return &Store{pool: db} }

// WithPageRead attaches the per-page read gate GetWorkspaceStats needs.
//
// ⚠ THE ROLL-UP IS REQUEST-SCOPED AND HAS NO SYSTEM READER, which is why there is no
// unexported all-pages twin here as there is in freshness. GetWorkspaceStats has exactly one
// caller — GET /v1/workspaces/{wsID}/analytics/pages, the SPA's Analytics screen — so gating the
// exported method gates the surface. If a background reader is ever added it must NOT reuse this
// method: it would report an empty workspace to a caller-less context.
func (s *Store) WithPageRead(a pageVisibility) *Store {
	s.access = a
	return s
}

// ErrNoPageReadGate is what the workspace roll-up returns when no gate was wired. It is a server
// misconfiguration, reported as one rather than answered with an unfiltered roll-up.
//
// ⚠ IT ERRORS RATHER THAN RETURNING AN EMPTY ROLL-UP. Zeroes here are not a neutral response —
// the SPA paints never_read_count and total_views as figures, so an empty roll-up is the positive
// claim "this workspace has no readership", which is a different false statement from the one
// this gate exists to stop.
var ErrNoPageReadGate = errors.New("analytics: no page-read gate wired (cmd/docs/main.go must call analyticsStore.WithPageRead)")

// ErrNotFound signals a RecordView for a page that is not in the caller's verified workspaces.
// Maps to 404 in the handler — no cross-tenant existence oracle.
var ErrNotFound = errors.New("analytics: page not found in an accessible workspace")

// RecordViewInWorkspaces is the SEC-4 L2 gate for RecordView: it records a view only if the
// page lives in one of the caller's verified workspaces (wsIDs = authz.WorkspaceIDs(ctx)).
//
// This is the in-method store gate — it holds ON ITS OWN, not because of the route enforcer.
// RecordView's page bump (UPDATE pages … WHERE id) was previously gated SOLELY by main.go's
// analyticsHandler.WithAccess(pageEnf) wiring; dropping that one line would have silently made
// the bump a live cross-tenant write. Now a foreign pageID resolves to ErrNotFound here, before
// any INSERT or bump, regardless of the wiring.
func (s *Store) RecordViewInWorkspaces(ctx context.Context, view PageView, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("analytics: no pool")
	}
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pages WHERE id = $1 AND workspace_id = ANY($2))`,
		view.PageID, wsIDs,
	).Scan(&exists); err != nil {
		return fmt.Errorf("analytics: scope check: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return s.RecordView(ctx, view)
}

// RecordView appends a row to page_views and bumps the cached
// counter on pages. Views under minDuration are dropped — accidental
// clicks shouldn't pollute analytics. Anonymous viewers come through
// as viewer_id="anonymous" so the schema's default is fine.
//
// Primitive: reached only via RecordViewInWorkspaces, which asserts the page is in the
// caller's verified workspaces first. Not to be wired to a route directly.
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
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- GATED IN-METHOD: RecordView is a primitive reached only via RecordViewInWorkspaces (above), which asserts the page is in the caller's verified workspaces (SELECT EXISTS … WHERE id = $1 AND workspace_id = ANY($2) → ErrNotFound) BEFORE this bump. The gate now holds on its own, independent of main.go's route-enforcer wiring (belt-and-suspenders: the route also enforces pageEnf.Require).
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
	// ⚠ `rows.Next()` RETURNS FALSE FOR TWO REASONS AND ONLY ONE OF THEM MEANS "THAT WAS ALL".
	// Without this, a stream that broke part-way left a SHORT series in a 200 response and the
	// Analytics screen drew it as the page's readership. MEASURED on real Postgres with the pgx
	// v5 this repo ships: `SET statement_timeout='150ms'` over a streaming read delivers 2 of 20
	// rows, `Query` has already returned nil and every `Scan` has already succeeded, and the
	// failure exists ONLY here (SQLSTATE 57014). A Scan failure is a different failure with a
	// different cause — the branch above handles that one and cannot stand in for this one.
	// rowstream_test.go holds both, and 33 of this repository's 36 read loops already did this.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: by day: %w", err)
	}

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
	// The same check on the second stream — a SEPARATE loop, so the one above does not cover it.
	// "Who read this document" is a list a missing name is invisible in.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: top viewers: %w", err)
	}
	return &out, nil
}

// rollupCap is how many rows each ranked cohort carries. It used to be a SQL `LIMIT 10` on each
// of two queries; it is applied in Go now, AFTER the visibility filter — see GetWorkspaceStats.
const rollupCap = 10

// GetWorkspaceStats rolls up the same window across every page in the workspace THAT THE CALLER
// MAY VIEW. The "never read" count is computed against the pages table (not page_views) so pages
// that were created without any traffic show up.
//
// ⚠ THIS IS THE FIFTH COPY OF THE PRIVATE-SPACE SEAM (#78 search/stale, #79 /ask, #80 freshness,
// #81 six MCP tools). Its single caller — GET /v1/workspaces/{wsID}/analytics/pages, which the
// SPA's Analytics screen reads — authorized the WORKSPACE and stopped, so a member with no grant
// on a private space received that space's page TITLES, their page ids (and therefore working
// /spaces/{space_id}/pages/{page_id} deep links) and their view counts. Measured on real
// Postgres in privatespace_realpg_test.go: the same caller is refused the same page with 403 one
// route over, at /spaces/{spaceID}/pages/{pageID}/analytics.
//
// ⚠⚠ THE FILTER MUST NOT SIT AFTER THE `LIMIT 10`, WHICH IS THE ONE PLACE THIS DIFFERS FROM #80.
// freshness had no LIMIT on its path, so filtering the returned list was complete. Truncating in
// SQL and filtering afterwards would hand back 10-minus-hidden rows: a workspace whose ten
// most-read pages are all private would render an EMPTY "Most read" beside a non-zero total. The
// ranked window is fetched WHOLE and capped in Go after filtering. [SHORT-LIST] holds it.
//
// ⚠ ONE QUERY NOW SERVES BOTH COHORTS. They were two statements differing only in ORDER BY, over
// the same rows; a single ranked fetch is the same set and cannot let the two drift apart. The
// `, pv.page_id` tiebreaker makes ties deterministic rather than leaving the two ends of one
// ordering to disagree about equally-viewed pages — #88's lesson on this repo's other ranked read.
//
// ⚠ COST, STATED RATHER THAN HIDDEN: visibility is decided PER PAGE by the permission engine (a
// page's tier depends on its own creator and page-level grants, not only its space), so this
// costs one AuthorizePageRead per viewed page plus one per never-read page. That is O(pages in
// the workspace) permission checks on a dashboard endpoint. Re-expressing the rule as a SQL
// predicate would bound it and is NOT done here: a second copy of the authorization rule that can
// drift from the engine is the failure this package is being fixed for. What would bound it
// honestly is a batch visibility lookup ON the permission engine, which does not exist yet.
func (s *Store) GetWorkspaceStats(ctx context.Context, workspaceID string, days int) (*WorkspaceReadStats, error) {
	// Fail-closed, and checked BEFORE the pool: an unwired gate is a server misconfiguration and
	// is reported as one. Mirrors freshness.GetStaleReport, including the nil receiver — a *Store
	// held in an interface field is a non-nil interface over a nil pointer, so a caller's own
	// nil-check does not protect this method.
	if s == nil || s.access == nil {
		return nil, ErrNoPageReadGate
	}
	if s.pool == nil {
		return nil, nil
	}
	if days <= 0 {
		days = 30
	}
	var out WorkspaceReadStats

	// The WHOLE ranked window, no LIMIT — the cap is applied after filtering.
	rows, err := s.pool.Query(ctx,
		`SELECT pv.page_id, MAX(p.title), COUNT(*)::int
        FROM page_views pv
        JOIN pages p ON p.id = pv.page_id
        WHERE pv.workspace_id = $1
          AND pv.created_at > NOW() - INTERVAL '1 day' * $2
        GROUP BY pv.page_id
        ORDER BY COUNT(*) DESC, pv.page_id`,
		workspaceID, days,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics: ranked pages: %w", err)
	}
	var ranked []ReadStats
	for rows.Next() {
		var r ReadStats
		if err := rows.Scan(&r.PageID, &r.Title, &r.TotalViews); err != nil {
			rows.Close()
			return nil, err
		}
		ranked = append(ranked, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: ranked pages: %w", err)
	}

	// Drop every page the caller may not VIEW, asking the same engine the by-page analytics route
	// asks, then total what is left. TotalViews is summed from the SURVIVING rows rather than read
	// from a workspace-wide COUNT — an unfiltered total beside a filtered list is itself a
	// disclosure ("there is readership you cannot see, and here is how much of it").
	visible := make([]ReadStats, 0, len(ranked))
	visibleIDs := make([]string, 0, len(ranked))
	for _, r := range ranked {
		found, canView := s.access.AuthorizePageRead(ctx, r.PageID)
		if !found || !canView {
			continue
		}
		visible = append(visible, r)
		visibleIDs = append(visibleIDs, r.PageID)
		out.TotalViews += r.TotalViews
	}

	// Most read: the head of the filtered ranking. Least read: its tail, reversed — the same set
	// the old ORDER BY ASC query returned, with > 0 views, so the "never read" cohort stays
	// separately driven and the UI's two sections stay independent.
	out.MostReadPages = append(out.MostReadPages, visible[:minInt(rollupCap, len(visible))]...)
	for i := len(visible) - 1; i >= 0 && len(out.LeastReadPages) < rollupCap; i-- {
		out.LeastReadPages = append(out.LeastReadPages, visible[i])
	}

	// Distinct viewers, over the visible pages only. Skipped entirely when nothing is visible —
	// `= ANY('{}')` is false for every row, so the query would be a round trip for a known 0.
	if len(visibleIDs) > 0 {
		if err := s.pool.QueryRow(ctx,
			`SELECT COUNT(DISTINCT viewer_id)::int
            FROM page_views
            WHERE workspace_id = $1
              AND created_at > NOW() - INTERVAL '1 day' * $2
              AND page_id = ANY($3)`,
			workspaceID, days, visibleIDs,
		).Scan(&out.UniqueViewers); err != nil {
			return nil, fmt.Errorf("analytics: unique viewers: %w", err)
		}
	}

	// Never read — pages that have never received a single view. Templates are excluded (they're
	// boilerplate, not content). The IDS are selected rather than a COUNT, because a count cannot
	// be filtered: the old statement counted private pages the caller cannot open, and a number is
	// exactly the shape no absence-of-a-row assertion can see.
	nrRows, err := s.pool.Query(ctx,
		`SELECT p.id FROM pages p
        LEFT JOIN page_views pv ON pv.page_id = p.id
        WHERE p.workspace_id = $1 AND p.is_template = false
          AND pv.id IS NULL`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("analytics: never read: %w", err)
	}
	var neverRead []string
	for nrRows.Next() {
		var id string
		if err := nrRows.Scan(&id); err != nil {
			nrRows.Close()
			return nil, err
		}
		neverRead = append(neverRead, id)
	}
	nrRows.Close()
	if err := nrRows.Err(); err != nil {
		return nil, fmt.Errorf("analytics: never read: %w", err)
	}
	for _, id := range neverRead {
		if found, canView := s.access.AuthorizePageRead(ctx, id); found && canView {
			out.NeverRead++
		}
	}

	return &out, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
