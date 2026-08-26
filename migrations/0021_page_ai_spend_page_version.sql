-- 0021_page_ai_spend_page_version.sql — WHICH REVISION A COMPLETION PAID FOR.
--
-- talyvor.higgsfield.app/products/docs sells "COST PER REVISION IN VERSION HISTORY", present
-- tense. page_versions has never carried a cost and page_ai_spend_events (0018) has never carried
-- a version: the table knows WHICH PAGE a completion was for and has never known WHICH REVISION.
-- A per-revision figure derived after the fact by comparing page_ai_spend_events.created_at
-- against page_versions.created_at would be a guess about money, so the revision is recorded at
-- BIND time, which is the one moment the writer knows it.
--
-- ⚠ THE COLUMN IS NULLABLE AND IS NOT BACKFILLED, AND THAT IS THE POINT. Every row already on
-- disk was written without a revision and no query can recover one — the fact was never captured.
-- A DEFAULT 0 or a timestamp-nearest backfill would turn "we do not know" into a specific claim
-- about which revision someone's money paid for. NULL says the true thing, and
-- Store.VersionCostSplit reports those rows as UNATTRIBUTABLE rather than folding them into a
-- revision or dropping them out of the total.
--
-- ⚠ THE VALUE IS MAX(version) + 1, NOT MAX(version), AND THAT WAS MEASURED RATHER THAN REASONED.
-- Probed on real Postgres: seed a page, save twice, and page_versions holds v1 = the state AFTER
-- save 1 and v2 = the state AFTER save 2; the seed state is in no row at all. So the AI work that
-- PRODUCED revision N is the spend bound between save N-1 and save N — while MAX(version) was
-- N-1. Recording MAX(version) would attribute every completion to the revision it came after
-- instead of the one it paid for, off by exactly one save in the direction nobody would notice:
-- both readings produce a plausible non-zero number on every row.
--
-- ⚠ A CONSEQUENCE, STATED NOT HIDDEN: spend bound after the newest save names a revision that does
-- not exist yet. It is PENDING, not lost, and appears on its revision the moment the next save
-- creates it. VersionCostSplit reports it as its own figure so the sum over visible revisions is
-- never mistaken for the page total.

ALTER TABLE page_ai_spend_events ADD COLUMN IF NOT EXISTS page_version INTEGER;

-- The per-revision rollup reads (page_id, page_version) and only ever wants priced rows; the
-- partial index keeps the pre-0021 rows, which can never satisfy the predicate, out of it.
CREATE INDEX IF NOT EXISTS idx_page_ai_spend_page_version
    ON page_ai_spend_events (page_id, page_version)
    WHERE page_version IS NOT NULL;
