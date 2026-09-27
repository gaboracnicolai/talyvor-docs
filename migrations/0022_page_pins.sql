-- 0022_page_pins.sql — B18.41: a member's pinned pages, kept on the server so they follow the member
-- to another browser or device (the suite kept them in browser storage until now).
--
-- Per workspace AND member: the member id is the one workspace_members gives the verified identity
-- in that workspace. A pin goes with its page (ON DELETE CASCADE), so a deleted page never lingers
-- in anyone's list.

CREATE TABLE IF NOT EXISTS page_pins (
    workspace_id TEXT        NOT NULL,
    member_id    TEXT        NOT NULL,
    page_id      TEXT        NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    pinned_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, member_id, page_id)
);

-- The one read: a member's pins in a workspace, newest first.
CREATE INDEX IF NOT EXISTS page_pins_member_idx ON page_pins (workspace_id, member_id, pinned_at DESC);
