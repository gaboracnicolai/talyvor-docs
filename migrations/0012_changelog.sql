-- 0012_changelog.sql — changelog page type + entries.
--
-- pages.page_type distinguishes a regular doc from a changelog
-- page. Default 'document' so the legacy rows stay correct
-- without a backfill.

ALTER TABLE pages
    ADD COLUMN IF NOT EXISTS page_type TEXT NOT NULL DEFAULT 'document';

CREATE TABLE IF NOT EXISTS changelog_entries (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id      TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    version      TEXT NOT NULL,
    title        TEXT NOT NULL,
    summary      TEXT NOT NULL DEFAULT '',
    type         TEXT NOT NULL DEFAULT 'feature',
    issue_ids    TEXT[] NOT NULL DEFAULT '{}',
    content      TEXT NOT NULL DEFAULT '{}',
    published_at TIMESTAMPTZ,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_changelog_page
    ON changelog_entries(page_id, published_at DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS idx_changelog_workspace
    ON changelog_entries(workspace_id, published_at DESC NULLS LAST)
    WHERE published_at IS NOT NULL;
