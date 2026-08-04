-- 0018_page_own_ai_spend.sql — A DOCUMENT'S OWN AI COST, KEPT SEPARATE FROM ITS ISSUES'.
--
-- THE GAP. pages.ai_cost_usd is the SUM OF LINKED TRACK ISSUES' COSTS. Docs' own AI calls
-- (write / summarize / grammar / shorter / longer / translate / title / ask / search) are tagged
-- by OPERATION — "docs-ai-write" — and never by page, so the cost of AI work ON a document has
-- never been attributable to that document anywhere. The marketing sentence "the cost of an
-- issue, a document or a change lands in one ledger" was true of the issue half only.
--
-- ⚠ WHY A SECOND COLUMN RATHER THAN ADDING TO ai_cost_usd. This is not a matter of taste; one
-- column cannot hold both numbers. ai_cost_usd is RECOMPUTED AND OVERWRITTEN on every sweep:
-- trackintegration.syncWorkspaceCosts sums the linked issues and calls UpdateAICost(pageID,
-- total) with an ABSOLUTE value. A page's own AI spend is ACCUMULATED from events that arrive
-- over time. Add the accumulation into the overwritten column and the very next sweep erases it —
-- silently, and only for pages that had AI work, which is the worst possible failure shape.
--
-- So the two numbers keep two columns, with two definitions that are written down here, in the
-- API, and in the page JSON:
--
--   ai_cost_usd      cost of the Track ISSUES linked to this page. Derived; recomputed from a
--                    complete set on every sweep; says nothing about AI work on the document.
--   own_ai_cost_usd  cost of AI operations performed ON this document. Accumulated exactly-once
--                    from page_ai_spend_events; never recomputed, never overwritten.
--
-- and their sum is exposed as a THIRD, explicitly-derived field (total_ai_cost_usd) rather than
-- by quietly widening either one. A number that changed meaning is worse than one that was
-- always narrow.
--
-- ⚠ HOW THIS INTERACTS WITH #51's INCOMPLETE-TOTAL RULE — it cannot reopen it, for two reasons:
--   1. Different column. syncWorkspaceCosts still computes and overwrites ai_cost_usd alone; its
--      "skip the write when the total is incomplete" branch is untouched and still governs the
--      only column it writes.
--   2. No total to be incomplete. own_ai_cost_usd is never computed as a sum-of-everything; it is
--      incremented by individual ledger rows that are inserted exactly-once. A partial pull from
--      Lens lands FEWER events, each of them correct, and the next pass lands the rest. There is
--      no equivalent of a partial total overwriting a complete one, because nothing is ever
--      overwritten.
--
-- ⚠ WHAT CANNOT BE ATTRIBUTED, stated rather than papered over. Two operations genuinely have no
-- single page and are therefore NOT recorded here: `docs-ai-ask` (answers across ctxPages — many
-- pages by construction) and `docs-search` (workspace-wide retrieval). Their cost remains visible
-- in Lens under its operation feature and is deliberately absent from every page. A page's
-- own_ai_cost_usd is therefore a LOWER BOUND on AI spend that touched it, and the API says so.

ALTER TABLE pages
    ADD COLUMN IF NOT EXISTS own_ai_cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0;

COMMENT ON COLUMN pages.ai_cost_usd IS
  'Cost of the Track ISSUES linked to this page. DERIVED — recomputed from a complete set of '
  'links on every sweep and overwritten. Says nothing about AI work performed on the document; '
  'that is own_ai_cost_usd. Do not add to this column: the next sweep overwrites it.';

COMMENT ON COLUMN pages.own_ai_cost_usd IS
  'Cost of AI operations performed ON this document, accumulated exactly-once from '
  'page_ai_spend_events. A LOWER BOUND: workspace-wide operations (docs-ai-ask, docs-search) have '
  'no single page and are excluded by design.';

-- The exactly-once ledger. Mirrors talyvor-track's ai_spend_events, and for the same reason: the
-- sync re-reads an overlapping window on every tick, so the primary key on request_id is what
-- makes a re-pull credit nothing.
--
-- ⚠ request_id IS LENS'S, NOT OURS. Lens returns X-Talyvor-Request-ID on every proxied
-- completion, and /v1/api/spend/by-request is keyed by the same value. Binding on it means the
-- page↔cost association is recorded by the process that MADE the call, at the moment it made it,
-- rather than reconstructed later from a string. See the migration note in
-- internal/lensintegration/spend.go for why the Track-style feature-string match does not fit a
-- page.
CREATE TABLE IF NOT EXISTS page_ai_spend_events (
    request_id   TEXT PRIMARY KEY,
    page_id      TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    operation    TEXT NOT NULL,
    -- NULL until the cost sync has resolved this request against Lens. A binding is written at
    -- call time (when the page is known) and priced later (when Lens can say what it cost), so
    -- an unpriced row is a normal intermediate state, not an error.
    cost_usd     DOUBLE PRECISION,
    tokens       INTEGER,
    priced_at    TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The sync's working set: bindings that still need a price, newest first.
CREATE INDEX IF NOT EXISTS idx_page_ai_spend_unpriced
    ON page_ai_spend_events (workspace_id, created_at DESC)
    WHERE cost_usd IS NULL;
