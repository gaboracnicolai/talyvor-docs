-- 0019_page_ai_spend_serve_source.sql — WHAT SERVED THE COMPLETION, SO A $0.00 ROW CAN BE READ.
--
-- THE GAP. page_ai_spend_events records what a Lens request cost and nothing about how it was
-- produced, and those are not independent facts. talyvor-lens writes the token_events row for a
-- CACHE-SERVED or NODE-SERVED response with `cost_usd = 0` hardcoded in the statement
-- (internal/alerts/alerts.go, insertCacheServeSQL — one INSERT shared by RecordCacheServe and
-- RecordNodeServe), because zero is TALYVOR'S PROVIDER cost: no upstream API was called. What the
-- requester owes for that serve lives in a DIFFERENT ledger, in a different unit — lxc_ledger for
-- a cache hit, lens_token_ledger (LENS, not USD) for a node serve. Lens says so in its own SQL
-- comments, excludes those rows from every provider-spend SUM it computes
-- (`SUM(cost_usd) FILTER (WHERE serve_source NOT LIKE 'cache_hit%')`), ships `serve_source` on
-- /v1/api/spend/by-request, and the handler that ships it instructs consumers to
-- "render cache rows as 'served from cache', not 'free'".
--
-- Docs's decoder dropped the field. So a cache-served completion landed here as a $0.00 row
-- indistinguishable from an upstream call that genuinely cost nothing — and because the
-- exactly-once guard is `cost_usd IS NULL`, pricing it at zero is PERMANENT: the row can never be
-- re-priced by a later pull that knows better. Measured on real Postgres through the shipped
-- sweep before this column existed: two bindings on one page, same operation, same tokens, one
-- served upstream and one from the cache, produced two ledger rows equal in every column.
--
-- ⚠ WHAT THIS MIGRATION DOES NOT DO, STATED SO NOBODY READS IT AS SETTLED. It does not change one
-- number. own_ai_cost_usd still accumulates exactly what Lens reports in cost_usd, so a
-- cache-served operation still contributes $0.00 to the page. WHETHER IT SHOULD — whether a
-- document's "AI writing cost" means Talyvor's provider cost (today's answer, now sayable) or the
-- requester's debit for the serve — is a pricing decision about a customer-visible number, and
-- this repo does not get to make it by choosing a column default. It is recorded in the queue for
-- Nicolai. What changes here is only that the fact stops being thrown away, so the decision is
-- still available to whoever makes it, and so today's zero is explainable rather than bare.

ALTER TABLE page_ai_spend_events
    ADD COLUMN IF NOT EXISTS serve_source TEXT NOT NULL DEFAULT '';

-- ⚠ EMPTY IS A THIRD STATE AND IT IS NOT 'upstream'. A pull from a Lens older than its migration
-- 0100 carries no serve_source at all, and so does every row priced before this column existed.
-- Defaulting those to 'upstream' would assert that a provider WAS paid — the one reading under
-- which a $0.00 row is genuinely free — on rows where nobody reported anything. Unpriced rows
-- (cost_usd IS NULL) are empty here for the same reason: nothing has been reported for them yet.
COMMENT ON COLUMN page_ai_spend_events.serve_source IS
  'What produced the bytes, as reported by Lens on /v1/api/spend/by-request: ''upstream'' (a '
  'provider was called and cost_usd is that provider cost), ''cache_hit_*'' or ''node'' (Talyvor '
  'paid no provider, so cost_usd is 0 and the requester''s debit is in another ledger), or '''' '
  '— NOT REPORTED: an older Lens, or a row not yet priced. Empty is never to be read as '
  '''upstream''.';

-- 0018 defined own_ai_cost_usd without the qualifier that makes it checkable. Its bytes are not
-- edited (the migration runner verifies the checksum of every applied file); the definition is
-- corrected here, where a reader of the live schema will find it.
COMMENT ON COLUMN pages.own_ai_cost_usd IS
  'TALYVOR''S PROVIDER COST for AI operations performed ON this document, accumulated '
  'exactly-once from page_ai_spend_events. A LOWER BOUND, for two distinct reasons: workspace-wide '
  'operations (docs-ai-ask, docs-search) have no single page and are excluded by design; and a '
  'completion Lens served from its cache or from a registered node cost Talyvor no provider money, '
  'so it adds 0 here while the requester is still debited in another ledger. '
  'page_ai_spend_events.serve_source is what distinguishes the two kinds of zero.';
