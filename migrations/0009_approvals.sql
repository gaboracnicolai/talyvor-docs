-- 0009_approvals.sql — document approval workflow.
--
-- pages.doc_status tracks the lifecycle: draft → in_review →
-- approved | rejected. The column lives on `pages` so editors
-- always have the current state without joining approval_requests.

ALTER TABLE pages
    ADD COLUMN IF NOT EXISTS doc_status TEXT NOT NULL DEFAULT 'draft';

-- Partial index — most pages live at 'draft' and we don't need to
-- spend an index entry on them.
CREATE INDEX IF NOT EXISTS idx_pages_doc_status
    ON pages(workspace_id, doc_status)
    WHERE doc_status != 'draft';

-- An approval_request is one round of review against one page. A
-- page can have many requests over its lifetime (e.g. revise +
-- re-request after rejection); the latest one is the live ask.
CREATE TABLE IF NOT EXISTS approval_requests (
    id           TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    page_id      TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    reviewers    TEXT[] NOT NULL,
    message      TEXT NOT NULL DEFAULT '',
    due_date     TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ DEFAULT NOW(),
    updated_at   TIMESTAMPTZ DEFAULT NOW()
);

-- One row per reviewer per request. UNIQUE prevents the
-- RequestApproval insert loop from accidentally creating dupes if
-- the same reviewer appears twice in the input slice.
CREATE TABLE IF NOT EXISTS review_decisions (
    id          TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    request_id  TEXT NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
    reviewer_id TEXT NOT NULL,
    decision    TEXT NOT NULL DEFAULT 'pending',
    comment     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE (request_id, reviewer_id)
);

CREATE INDEX IF NOT EXISTS idx_approval_requests_page
    ON approval_requests(page_id, created_at DESC);

-- Reviewer's inbox — pending decisions assigned to them. Partial
-- index keeps it tight since most decisions land in
-- approved/rejected once a request is closed.
CREATE INDEX IF NOT EXISTS idx_review_decisions_reviewer
    ON review_decisions(reviewer_id, decision)
    WHERE decision = 'pending';
