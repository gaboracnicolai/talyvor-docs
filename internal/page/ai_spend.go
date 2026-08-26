package page

import (
	"context"
	"errors"
	"fmt"
)

// A DOCUMENT'S OWN AI COST — the two halves, and why they are two calls.
//
// BindAISpend records that a Lens request belonged to a page. It runs at CALL TIME, in the
// handler that knows the page, and it stores no cost — Docs does not know what a completion cost
// and must not guess one. (The VS Code extension guesses, with a hardcoded Haiku rate applied to
// every model; the number it produces is wrong by 4x on the default model and 20x on Opus. Not a
// mistake to copy.)
//
// PriceAISpend runs later, from the sync that can ask Lens what the request actually cost, and it
// is the only thing that moves money into pages.own_ai_cost_usd.
//
// Splitting them is what makes the ledger honest: the binding is a fact Docs observed, the price
// is a fact Lens observed, and neither process invents the other's.

// ErrNoBinding distinguishes "this request was not ours" from a failure. The sync pulls every
// request in the workspace, most of which are not Docs page operations at all, so a miss is the
// common case and must not read as an error.
var ErrNoBinding = errors.New("page: no AI spend binding for that request")

// ErrPageWorkspaceMismatch reports a binding whose page does not live in the workspace the
// request was billed to. Distinct from ErrNoBinding: that one is a request nobody claimed, this
// one is a request claimed by the wrong tenant.
var ErrPageWorkspaceMismatch = errors.New("page: the page named by an AI spend binding is not in the workspace being billed")

// BindAISpend associates a Lens request_id with the page the operation was performed on.
//
// Idempotent by primary key: a retried handler, or a duplicate request id, records once. It
// deliberately does NOT touch pages.own_ai_cost_usd — an unpriced binding must not move a
// customer-visible number.
//
// ⚠ THE PAGE MUST BE IN THE WORKSPACE BEING BILLED, AND THE ROW IS WHY. workspace_id is the
// workspace that PAID Lens for this completion; page_id is what PriceAISpend will roll the money
// onto, with `WHERE p.id = priced.page_id` and no workspace predicate. A row where the two
// disagree is not a partial fact to be repaired later — it is money one tenant paid appearing on
// another tenant's document, and the FOREIGN KEY cannot see it because the page genuinely exists.
// The two ids arrive from different places (the path and the request body) and were authorized by
// different gates, so nothing upstream had both in one hand until here.
//
// ⚠ THE PREDICATE IS `ok`, NOT RowsAffected, and that is not style. `ON CONFLICT DO NOTHING`
// already affects zero rows for the ordinary re-bind, so a RowsAffected==0 branch would report
// idempotence and a refused mismatch as the same outcome — and the mismatch is the one that must
// be loud. `ok` counts the page rows that satisfied the join, so it separates them.
func (s *Store) BindAISpend(ctx context.Context, requestID, pageID, workspaceID, operation string) error {
	if requestID == "" || pageID == "" || workspaceID == "" || operation == "" {
		return errors.New("page: BindAISpend requires request_id, page_id, workspace_id, operation")
	}
	var ok int
	if err := s.pool.QueryRow(ctx, `
        WITH ok AS (
            SELECT 1 FROM pages WHERE id = $2 AND workspace_id = $3
        ),
        ins AS (
            INSERT INTO page_ai_spend_events (request_id, page_id, workspace_id, operation, page_version)
            SELECT $1, $2, $3, $4,
                   -- ⚠ MAX + 1: the revision this work is GOING INTO, not the one it came after.
                   -- page_versions row N is the state AFTER save N (measured, not read — see
                   -- migrations/0021), so the spend that produced revision N is bound while
                   -- MAX(version) is N-1. Recording MAX(version) would put every completion on
                   -- the previous revision, off by one save, with a plausible number on every row.
                   COALESCE((SELECT MAX(version) FROM page_versions WHERE page_id = $2), 0) + 1
              FROM ok
            ON CONFLICT (request_id) DO NOTHING
            RETURNING 1
        )
        SELECT count(*) FROM ok`,
		requestID, pageID, workspaceID, operation,
	).Scan(&ok); err != nil {
		return fmt.Errorf("page: bind ai spend: %w", err)
	}
	if ok == 0 {
		return fmt.Errorf("%w: page %s, workspace %s", ErrPageWorkspaceMismatch, pageID, workspaceID)
	}
	return nil
}

// PriceAISpend lands the cost of one Lens request on the page it was bound to, EXACTLY ONCE.
//
// ⚠ IT DOES NOT TOUCH updated_at, AND IT USED TO — THIS IS THE THIRD COPY OF THE SEAM THE
// RecordView BLOCK IN store.go DESCRIBES. GetStalePages keys on pages.updated_at, so landing a
// cost here re-dated the document: measured on real Postgres, a page 200 days past a 30-day TTL
// dropped off the stale list when a $0.0031 `docs-ai-summarize` was priced. The operation that
// paid for it need not have changed anything — summarize and title-suggest are read-only — and
// this sweep runs minutes-to-hours after the call, so the timestamp was not even the moment of
// the operation. For the features that DO change the page, the user's save goes through Update,
// which bumps the column itself. The sibling writer UpdateAICost never touched it.
// Pinned by aispend_staleness_test.go.
//
// ⚠ THE EXACTLY-ONCE GUARANTEE IS THE `cost_usd IS NULL` PREDICATE, not the caller's diligence.
// The sync re-reads an overlapping window on every tick by design, so the same request arrives
// repeatedly. The UPDATE matches only a still-unpriced row, and pages.own_ai_cost_usd is
// incremented in the same statement from that row — so a second call updates zero rows, adds
// zero, and reports landed=false.
//
// Returns:
//
//	landed=true   the cost was applied to the page just now
//	landed=false  with err==nil          already priced (a re-pull), nothing changed
//	ErrNoBinding                          this request was never bound to a page — NOT an error
//	                                      condition, just a request that was not a page operation
//
// ⚠ THE THIRD LINE WAS A PROMISE THE CODE COULD NOT KEEP, AND THE STATEMENT ALREADY HELD THE
// ANSWER. The `bound` CTE below exists for exactly this decision and its count was scanned into
// `new(int)` — discarded — so ErrNoBinding had ZERO producers in the repository and
// `errors.Is(err, ErrNoBinding)` was false for every possible input. "Never ours" and "already
// priced" came back as the same `(false, nil)`, which is the one distinction a reconciliation of
// this ledger has to make. Fixing it moves no money: `landed` and the roll-up are untouched.
func (s *Store) PriceAISpend(ctx context.Context, requestID string, costUSD float64, tokens int) (landed bool, err error) {
	return s.PriceAISpendWithServeSource(ctx, requestID, costUSD, tokens, "")
}

// PriceAISpendWithServeSource is PriceAISpend plus the one fact that says whether a $0.00 price is
// a completion that cost nothing or a completion Talyvor did not pay a provider for.
//
// ⚠ WHY THE FOUR-ARGUMENT FORM SURVIVES RATHER THAN EVERY CALLER GAINING AN EMPTY STRING. The
// sweep is the only caller that has a serve_source to pass — it is a field of Lens's pull — and it
// is bound to THIS method by the PageSpendStore interface, so it cannot silently fall back to the
// short form. Every other caller (the store's own guards, the attribution and billing tests) is
// asserting something about money or tenancy and has no serve_source in hand; making them write
// `""` would spell "not reported" in eleven places instead of one, and each of those is a seam a
// later reader could mistake for a claim about how the completion was served.
//
// serveSource is recorded VERBATIM, including empty. Empty means NOT REPORTED and must never be
// normalised to "upstream": that value asserts a provider was paid, which is the whole question.
func (s *Store) PriceAISpendWithServeSource(ctx context.Context, requestID string, costUSD float64, tokens int, serveSource string) (landed bool, err error) {
	if requestID == "" {
		return false, errors.New("page: PriceAISpend requires request_id")
	}
	var updated, bound int
	qErr := s.pool.QueryRow(ctx, `
        WITH priced AS (
            UPDATE page_ai_spend_events
               SET cost_usd = $2, tokens = $3, priced_at = NOW(), serve_source = $4
             WHERE request_id = $1 AND cost_usd IS NULL
            RETURNING page_id, cost_usd
        ),
        rolled AS (
            UPDATE pages p
               SET own_ai_cost_usd = p.own_ai_cost_usd + priced.cost_usd
              FROM priced
             WHERE p.id = priced.page_id
            RETURNING p.id
        ),
        bound AS (
            SELECT 1 FROM page_ai_spend_events WHERE request_id = $1
        )
        SELECT (SELECT count(*) FROM rolled), (SELECT count(*) FROM bound)`,
		requestID, costUSD, tokens, serveSource,
	).Scan(&updated, &bound)
	if qErr != nil {
		return false, fmt.Errorf("page: price ai spend: %w", qErr)
	}
	// ⚠ THE CTE READS THE PRE-STATEMENT SNAPSHOT, WHICH IS WHY `bound` SURVIVES THE UPDATE ABOVE
	// IT. `priced` only ever SETs columns on the binding row — it never deletes one — so a request
	// this sweep just priced still counts as bound, and only a request with no row at all reaches
	// zero. request_id is the table's conflict key, so the count is 0 or 1, never more.
	if bound == 0 {
		return false, fmt.Errorf("%w: request %s", ErrNoBinding, requestID)
	}
	return updated > 0, nil
}

// UnpricedRequestIDs returns the bindings still awaiting a price for one workspace, newest first.
// The sync uses it to decide which of Lens's per-request rows are worth looking at — Docs is one
// of several tenants of a Lens workspace, so most rows in a pull belong to something else.
func (s *Store) UnpricedRequestIDs(ctx context.Context, workspaceID string, limit int) ([]string, error) {
	if workspaceID == "" {
		return nil, errors.New("page: UnpricedRequestIDs requires workspace_id")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
        SELECT request_id FROM page_ai_spend_events
         WHERE workspace_id = $1 AND cost_usd IS NULL
         ORDER BY created_at DESC LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("page: unpriced request ids: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OwnAICost reads the page's own accumulated AI cost. Separate from the linked-issue total, which
// UpdateAICost owns; see migration 0018 for why the two cannot share a column.
func (s *Store) OwnAICost(ctx context.Context, pageID string) (float64, error) {
	var v float64
	if err := s.pool.QueryRow(ctx,
		`SELECT own_ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&v); err != nil {
		return 0, fmt.Errorf("page: own ai cost: %w", err)
	}
	return v, nil
}

// UnpricedWorkspaces returns the workspaces that have bindings still awaiting a price.
//
// ⚠ THE WORKSPACE LIST COMES FROM THE DATA, not from configuration. That is deliberate and it is
// what makes the pricing sweep structurally incapable of the single-workspace bug #51 fixed in the
// linked-issue sweep: there is no configured workspace anywhere in this path to be pinned to.
func (s *Store) UnpricedWorkspaces(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
        SELECT DISTINCT workspace_id FROM page_ai_spend_events
         WHERE cost_usd IS NULL ORDER BY workspace_id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("page: unpriced workspaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// VersionCostSplit is the whole of a page's priced AI spend, divided by whether version history
// can show it.
//
// ⚠ IT EXISTS BECAUSE A PER-REVISION FIGURE THAT DOES NOT ADD UP IS A LIE ABOUT MONEY THAT LOOKS
// LIKE A FEATURE. Summing the ai_cost_usd column down a page's version history gives Attributed
// and nothing else, and a reader has no way to tell that from the page's own_ai_cost_usd. The two
// differ for two reasons that are both normal:
//
//	Pending        spend bound after the newest save names a revision that does not exist yet. It
//	               is not lost — it appears on its revision the moment the next save creates it.
//	Unattributable every page_ai_spend_events row written before migration 0021 carries no
//	               revision, and no query can recover one: the fact was never captured. Reporting
//	               these as 0 on some revision, or dropping them, would both be claims nobody
//	               measured.
//
// Attributed + Pending + Unattributable == the page's own_ai_cost_usd, and PageTotal is read from
// that column rather than recomputed so the two can be compared rather than assumed equal.
type VersionCostSplit struct {
	Attributed     float64
	Pending        float64
	Unattributable float64
	PageTotal      float64
}

// VersionCostSplit reports the split for one page, refusing pages outside the caller's workspaces.
func (s *Store) VersionCostSplit(ctx context.Context, pageID string, wsIDs []string) (VersionCostSplit, error) {
	if err := s.assertInWorkspaces(ctx, pageID, wsIDs); err != nil {
		return VersionCostSplit{}, err
	}
	var out VersionCostSplit
	if err := s.pool.QueryRow(ctx, `
        WITH maxv AS (
            SELECT COALESCE(MAX(version), 0) AS v FROM page_versions WHERE page_id = $1
        ),
        priced AS (
            SELECT page_version, cost_usd
              FROM page_ai_spend_events
             WHERE page_id = $1 AND cost_usd IS NOT NULL
        )
        SELECT
            COALESCE((SELECT SUM(cost_usd) FROM priced, maxv
                       WHERE page_version IS NOT NULL AND page_version BETWEEN 1 AND maxv.v), 0),
            COALESCE((SELECT SUM(cost_usd) FROM priced, maxv
                       WHERE page_version IS NOT NULL AND page_version > maxv.v), 0),
            COALESCE((SELECT SUM(cost_usd) FROM priced
                       WHERE page_version IS NULL), 0),
            COALESCE((SELECT own_ai_cost_usd FROM pages WHERE id = $1), 0)`,
		pageID,
	).Scan(&out.Attributed, &out.Pending, &out.Unattributable, &out.PageTotal); err != nil {
		return VersionCostSplit{}, fmt.Errorf("page: version cost split: %w", err)
	}
	return out, nil
}
