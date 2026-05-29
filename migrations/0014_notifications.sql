-- 0014_notifications.sql
--
-- Email-notification support for Docs.
--
-- Docs is intentionally stateless about identity — users are opaque member IDs
-- supplied via the X-Member-Id header, and there is no users table. To send
-- email we need addresses, so this migration adds a self-contained directory:
--
--   notification_recipients  member_id -> email (+ display name)
--
-- Populating it is a separate integration concern (sync/seed from the identity
-- source); the dispatcher resolves addresses from here and BEST-EFFORT SKIPS
-- any member without a row, so an empty table simply means no email is sent.
--
-- notification_preferences is the per-member, per-event opt-out (absence of a
-- row = opted in), mirroring the model used in Track.

CREATE TABLE IF NOT EXISTS notification_recipients (
    member_id  TEXT PRIMARY KEY,
    email      TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS notification_preferences (
    member_id     TEXT    NOT NULL,
    event_type    TEXT    NOT NULL,
    email_enabled BOOLEAN NOT NULL DEFAULT true,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (member_id, event_type)
);
