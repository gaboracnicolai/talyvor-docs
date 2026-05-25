-- 0007_databases.sql — inline structured-data blocks.
--
-- Three tables back the database block: the schema declaration on
-- `databases`, the row data on `database_rows` (values as JSONB
-- because columns are user-defined), and per-database views on
-- `database_views`. Cascade-deletes keep everything tied to the
-- parent page so deleting a page wipes its embedded databases too.

CREATE TABLE IF NOT EXISTS databases (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id      TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT 'Untitled Database',
    schema       JSONB NOT NULL DEFAULT '[]',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

-- database_rows holds the row content. We keep values JSONB so the
-- column set can change without altering the table schema.
CREATE TABLE IF NOT EXISTS database_rows (
    id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    database_id TEXT NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    values      JSONB NOT NULL DEFAULT '{}',
    position    FLOAT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

-- database_views stores the saved view configurations (filters,
-- sort, group-by, hidden columns). hidden_cols is a text[] so
-- per-view column hiding doesn't bloat the row.
CREATE TABLE IF NOT EXISTS database_views (
    id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    database_id TEXT NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    name        TEXT NOT NULL DEFAULT 'Table',
    type        TEXT NOT NULL DEFAULT 'table',
    filters     JSONB NOT NULL DEFAULT '[]',
    sort_by     TEXT NOT NULL DEFAULT '',
    sort_dir    TEXT NOT NULL DEFAULT 'asc',
    group_by    TEXT NOT NULL DEFAULT '',
    hidden_cols TEXT[] NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_db_rows_database
    ON database_rows(database_id, position);
CREATE INDEX IF NOT EXISTS idx_db_views_database
    ON database_views(database_id);
