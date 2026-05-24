-- 0006_permissions.sql — Phase 8 access control + public sharing.
--
-- permissions backs the in-app sharing model: workspace members /
-- teams / "everyone" granted view, comment, edit, or admin on a
-- space or page. UNIQUE(resource, subject) makes Grant idempotent
-- — re-granting the same subject simply updates the access level.
CREATE TABLE IF NOT EXISTS permissions (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    subject_type  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    access        TEXT NOT NULL,
    workspace_id  TEXT NOT NULL,
    granted_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (resource_type, resource_id, subject_type, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_permissions_resource
    ON permissions(resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_permissions_subject
    ON permissions(subject_type, subject_id);

-- share_links: the public-token surface. The token is a UUID
-- (non-sequential) so attackers can't enumerate links. password_hash
-- is bcrypt; the API never serialises the column. ON DELETE CASCADE
-- means deleting a page silently invalidates all its share links.
CREATE TABLE IF NOT EXISTS share_links (
    id            TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id       TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id  TEXT NOT NULL,
    token         TEXT UNIQUE NOT NULL,
    access        TEXT NOT NULL DEFAULT 'view',
    expires_at    TIMESTAMPTZ,
    password_hash TEXT,
    view_count    INTEGER NOT NULL DEFAULT 0,
    created_by    TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_share_links_token
    ON share_links(token);
CREATE INDEX IF NOT EXISTS idx_share_links_page
    ON share_links(page_id);
