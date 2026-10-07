-- 0023_teams.sql — B28.446: teams a page or space can be shared with.
--
-- Docs owns these rows: Track has a teams table but no team membership, so nothing upstream can
-- tell Docs who is in a team. A team lives in one workspace and its members are workspace member
-- ids (the id workspace_members gives a verified identity in that workspace). The team's creator
-- manages its roster. A permissions row with subject_type = 'team' names teams.id.

CREATE TABLE IF NOT EXISTS teams (
    id           TEXT        PRIMARY KEY DEFAULT gen_random_uuid()::text,
    workspace_id TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    created_by   TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS team_members (
    team_id      TEXT        NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    workspace_id TEXT        NOT NULL,
    member_id    TEXT        NOT NULL,
    added_by     TEXT        NOT NULL,
    added_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, member_id)
);

-- The read the rule engine runs whenever a check meets a team grant: a member's teams in a workspace.
CREATE INDEX IF NOT EXISTS team_members_member_idx ON team_members (workspace_id, member_id);
