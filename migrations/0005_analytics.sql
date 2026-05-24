-- 0005_analytics.sql — Phase 7 readership analytics.
--
-- page_views is the append-only per-view event log. The aggregate
-- counters on pages.view_count + pages.last_viewed_at remain the
-- fast path for cheap "page info" displays; this table backs the
-- richer per-viewer / per-day analytics surfaces.
CREATE TABLE IF NOT EXISTS page_views (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id      TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    viewer_id    TEXT NOT NULL DEFAULT 'anonymous',
    viewer_name  TEXT NOT NULL DEFAULT '',
    duration_sec INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

-- Per-page chronological scan; backs the per-page analytics panel.
CREATE INDEX IF NOT EXISTS idx_page_views_page
    ON page_views(page_id, created_at DESC);

-- Workspace-scoped queries (most-read / least-read / day buckets).
CREATE INDEX IF NOT EXISTS idx_page_views_workspace
    ON page_views(workspace_id, created_at DESC);

-- Top-viewers per page goes through this index — a single bookmark
-- to GROUP BY viewer_id without rescanning page_views per page.
CREATE INDEX IF NOT EXISTS idx_page_views_viewer
    ON page_views(viewer_id, page_id);
