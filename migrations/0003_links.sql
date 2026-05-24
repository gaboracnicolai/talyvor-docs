-- Cross-product page ↔ issue links. workspace_id is opaque (no FK)
-- so this table can live alongside a Track-less Docs deployment;
-- when Track is wired the IDs match Track's issues.id.
--
-- UNIQUE(page_id, issue_id, link_type) backs the idempotent Upsert.
-- Three link types are reserved:
--   - embed:   issue rendered inline in the page body
--   - mention: link from the right-panel backlinks UI
--   - spec:    page is the spec for the issue (manual annotation)

CREATE TABLE IF NOT EXISTS page_links (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id      TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    issue_id     TEXT NOT NULL,
    link_type    TEXT NOT NULL DEFAULT 'embed',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (page_id, issue_id, link_type)
);

CREATE INDEX IF NOT EXISTS idx_page_links_page  ON page_links(page_id);
CREATE INDEX IF NOT EXISTS idx_page_links_issue ON page_links(issue_id);
