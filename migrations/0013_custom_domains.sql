-- 0013_custom_domains.sql — custom domain routing.
--
-- A custom domain maps an external hostname (docs.company.com) to
-- a workspace (and optionally a single space). The DNS TXT-record
-- verification proves the user controls the hostname before we
-- start serving content.

CREATE TABLE IF NOT EXISTS custom_domains (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id TEXT NOT NULL,
    domain       TEXT UNIQUE NOT NULL,
    space_id     TEXT REFERENCES spaces(id) ON DELETE SET NULL,
    verified     BOOLEAN NOT NULL DEFAULT false,
    verify_token TEXT NOT NULL,
    ssl_status   TEXT NOT NULL DEFAULT 'pending',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_custom_domains_workspace
    ON custom_domains(workspace_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_custom_domains_domain
    ON custom_domains(domain);
