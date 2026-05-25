-- 0010_locking.sql — soft page locks.
--
-- Three columns on `pages`: a boolean flag, the locker's member
-- ID, and the timestamp of the lock. Soft means the lock is stored
-- in the DB (survives restarts) rather than in-memory — when the
-- server restarts users see exactly the lock state they left.

ALTER TABLE pages ADD COLUMN IF NOT EXISTS locked BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE pages ADD COLUMN IF NOT EXISTS locked_by TEXT;
ALTER TABLE pages ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ;

-- Partial index — locked rows are a small fraction of pages, and
-- the lookup we care about ("who has a lock right now?") only
-- needs the locked rows.
CREATE INDEX IF NOT EXISTS idx_pages_locked
    ON pages(locked_by)
    WHERE locked = true;
