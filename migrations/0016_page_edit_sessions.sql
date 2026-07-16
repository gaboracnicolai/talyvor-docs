-- 0016_page_edit_sessions.sql — the single-writer edit session (Option A's policy seam).
--
-- One row per page names the current WRITER: who holds the editing slot right now, when they
-- took it, and their last heartbeat. A session is LIVE iff last_heartbeat > now() - TTL
-- (the TTL lives in code, internal/editsession). A live session held by someone else blocks a
-- non-holder's save ("locked by <holder>"); an expired/absent session is claimable via
-- Acquire/Takeover. This is the ONE place the ephemeral single-writer decision lives:
-- Option B later swaps the policy (single holder → many concurrent writers + presence + CRDT
-- merge) by replacing internal/editsession.MayWrite, without touching the save path, the
-- approval gate, the manual pagelock, or the append-only version history.
--
-- workspace_id is denormalized from the page so a session row is self-describing about its
-- tenant and every edit-session op scopes on the SERVER-authorized workspace. page_id is the
-- primary key (one writer slot per page); ON DELETE CASCADE drops the session with its page.

CREATE TABLE IF NOT EXISTS page_edit_sessions (
    page_id        TEXT PRIMARY KEY REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id   TEXT NOT NULL,
    holder         TEXT NOT NULL,
    acquired_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Tenancy scoping filters on (workspace_id, page_id); the takeover/liveness check reads by
-- last_heartbeat.
CREATE INDEX IF NOT EXISTS idx_page_edit_sessions_ws
    ON page_edit_sessions (workspace_id, page_id);
