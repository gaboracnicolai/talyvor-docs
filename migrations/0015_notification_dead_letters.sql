-- 0015_notification_dead_letters.sql
--
-- Dead-letter table for email notifications. When the async delivery queue
-- exhausts all retry attempts for a message (e.g. SMTP down for an extended
-- window), the give-up is recorded here instead of being silently dropped, so
-- an admin can see which notifications failed and follow up.
--
-- Metadata only: recipients, subject, attempt count, and the last error. The
-- rendered body is intentionally NOT stored — this is an ops surface, not a
-- copy of notification content.
--
-- Additive and idempotent (CREATE TABLE IF NOT EXISTS); inert until email is
-- enabled (EMAIL_ENABLED) and a delivery permanently fails.

CREATE TABLE IF NOT EXISTS notification_dead_letters (
    id          BIGSERIAL   PRIMARY KEY,
    recipients  TEXT[]      NOT NULL,
    subject     TEXT        NOT NULL,
    attempts    INTEGER     NOT NULL,
    last_error  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notification_dead_letters_created_at
    ON notification_dead_letters (created_at DESC);
