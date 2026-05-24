-- Core extensions + spaces table. Spaces are the top-level grouping
-- in Docs (one workspace, many spaces); pages nest under spaces.
-- workspace_id is intentionally untyped so this service can stand
-- alone — Track owns the workspaces table; Docs treats workspace_id
-- as an opaque tenant key.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
CREATE EXTENSION IF NOT EXISTS "vector";

CREATE TABLE IF NOT EXISTS spaces (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id TEXT NOT NULL,
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    icon         TEXT NOT NULL DEFAULT '📄',
    color        TEXT NOT NULL DEFAULT '#6366f1',
    private      BOOLEAN NOT NULL DEFAULT false,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (workspace_id, slug)
);
