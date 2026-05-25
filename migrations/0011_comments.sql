-- 0011_comments.sql — threaded comments with resolution tracking.
--
-- Extends the existing page_comments table with thread metadata,
-- a typed parent link, an explicit resolved_at timestamp, and an
-- author_name display column. thread_id groups top-level + replies;
-- a comment's thread_id equals its own id when it's the head of a
-- thread.

ALTER TABLE page_comments
    ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
ALTER TABLE page_comments
    ADD COLUMN IF NOT EXISTS thread_id TEXT;
ALTER TABLE page_comments
    ADD COLUMN IF NOT EXISTS parent_id TEXT
        REFERENCES page_comments(id) ON DELETE CASCADE;
ALTER TABLE page_comments
    ADD COLUMN IF NOT EXISTS author_name TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_comments_thread
    ON page_comments(thread_id) WHERE thread_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_comments_parent
    ON page_comments(parent_id) WHERE parent_id IS NOT NULL;

-- Backfill thread_id for legacy rows so the new code can rely on
-- thread_id being non-null. Top-level legacy comments become their
-- own thread.
UPDATE page_comments SET thread_id = id WHERE thread_id IS NULL;
