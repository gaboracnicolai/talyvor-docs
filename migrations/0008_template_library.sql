-- 0008_template_library.sql — workspace template gallery.
--
-- Built-in templates live in Go code (so the gallery still works
-- without any DB rows); workspace-specific custom templates land
-- here. is_built_in flags the source: built-ins exist in code only,
-- but the column is kept on the table so future schema migrations
-- can opt to seed them.

CREATE TABLE IF NOT EXISTS library_templates (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    category     TEXT NOT NULL,
    icon         TEXT NOT NULL DEFAULT '📄',
    tags         TEXT[] NOT NULL DEFAULT '{}',
    content      TEXT NOT NULL DEFAULT '{}',
    content_text TEXT NOT NULL DEFAULT '',
    is_built_in  BOOLEAN NOT NULL DEFAULT false,
    workspace_id TEXT,
    created_by   TEXT,
    use_count    INTEGER NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_lib_templates_category
    ON library_templates(category, is_built_in);
CREATE INDEX IF NOT EXISTS idx_lib_templates_workspace
    ON library_templates(workspace_id)
    WHERE workspace_id IS NOT NULL;
