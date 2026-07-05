-- 0014_workspace_members.sql — SEC-4 A0b PR-2: the membership source Docs lacks.
--
-- Docs owns no members table (workspace_id is an external Track id). This is Docs's
-- LOCAL mirror of a workspace's roster, full-pulled from Track's service endpoint
-- (GET /v1/service/members) by internal/trackintegration.SyncMembers. SEC-4 Layer 1
-- (PR-3) resolves a verified x-user-email against this table to decide workspace access.
--
-- Primary key (workspace_id, email): one row per member per workspace, upserted on sync,
-- pruned when a member leaves (full-pull reconcile — Track emits no change signal).
CREATE TABLE IF NOT EXISTS workspace_members (
    workspace_id TEXT NOT NULL,
    email        TEXT NOT NULL,
    role         TEXT NOT NULL,
    member_id    TEXT NOT NULL,
    synced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, email)
);

-- Layer 1 resolves by email across workspaces; the PK covers (workspace_id, email) but
-- an email-first lookup benefits from this index.
CREATE INDEX IF NOT EXISTS idx_workspace_members_email ON workspace_members (email);
