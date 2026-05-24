-- Pages, versions, and inline comments.
--
-- pages.content holds the canonical ProseMirror JSON; content_text
-- is the plain-text projection that drives full-text search. We
-- denormalise so search reads never have to parse JSON, and the
-- tsvector index can be GIN-backed without per-row CPU.
--
-- linked_issues is a TEXT[] of Track issue IDs; Phase 2 wires the
-- bidirectional link via the Lens-style integration pattern.

CREATE TABLE IF NOT EXISTS pages (
    id               TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    space_id         TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    workspace_id     TEXT NOT NULL,
    parent_id        TEXT REFERENCES pages(id) ON DELETE SET NULL,
    title            TEXT NOT NULL DEFAULT 'Untitled',
    slug             TEXT NOT NULL,
    content          TEXT NOT NULL DEFAULT '{}',
    content_text     TEXT NOT NULL DEFAULT '',
    icon             TEXT NOT NULL DEFAULT '',
    cover_url        TEXT NOT NULL DEFAULT '',
    position         FLOAT NOT NULL DEFAULT 0,
    depth            INTEGER NOT NULL DEFAULT 0,
    is_template      BOOLEAN NOT NULL DEFAULT false,
    created_by       TEXT NOT NULL DEFAULT '',
    updated_by       TEXT NOT NULL DEFAULT '',
    linked_issues    TEXT[] NOT NULL DEFAULT '{}',
    ai_cost_usd      FLOAT NOT NULL DEFAULT 0,
    view_count       INTEGER NOT NULL DEFAULT 0,
    last_viewed_at   TIMESTAMPTZ,
    last_verified_at TIMESTAMPTZ,
    verified_by      TEXT,
    stale_after_days INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ DEFAULT NOW(),
    updated_at       TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (space_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_pages_space ON pages(space_id, position);
CREATE INDEX IF NOT EXISTS idx_pages_parent ON pages(parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_pages_search ON pages USING gin(
    to_tsvector('english', title || ' ' || content_text)
);
CREATE INDEX IF NOT EXISTS idx_pages_stale ON pages(workspace_id, updated_at)
    WHERE stale_after_days > 0;

CREATE TABLE IF NOT EXISTS page_versions (
    id         TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id    TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    version    INTEGER NOT NULL,
    title      TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (page_id, version)
);

CREATE INDEX IF NOT EXISTS idx_versions_page ON page_versions(page_id, version DESC);

CREATE TABLE IF NOT EXISTS page_comments (
    id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id     TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    block_id    TEXT,
    author_id   TEXT NOT NULL,
    content     TEXT NOT NULL,
    resolved    BOOLEAN NOT NULL DEFAULT false,
    resolved_by TEXT,
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_comments_page ON page_comments(page_id, resolved);

CREATE TABLE IF NOT EXISTS blocks (
    id         TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id    TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    content    TEXT NOT NULL DEFAULT '{}',
    position   FLOAT NOT NULL DEFAULT 0,
    parent_id  TEXT REFERENCES blocks(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_blocks_page ON blocks(page_id, position);
