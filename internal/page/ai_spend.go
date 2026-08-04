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

// BindAISpend associates a Lens request_id with the page the operation was performed on.
//
// Idempotent by primary key: a retried handler, or a duplicate request id, records once. It
// deliberately does NOT touch pages.own_ai_cost_usd — an unpriced binding must not move a
// customer-visible number.
func (s *Store) BindAISpend(ctx context.Context, requestID, pageID, workspaceID, operation string) error {
	if requestID == "" || pageID == "" || workspaceID == "" || operation == "" {
		return errors.New("page: BindAISpend requires request_id, page_id, workspace_id, operation")
	}
	_, err := s.pool.Exec(ctx, `
        INSERT INTO page_ai_spend_events (request_id, page_id, workspace_id, operation)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (request_id) DO NOTHING`,
		requestID, pageID, workspaceID, operation)
	if err != nil {
		return fmt.Errorf("page: bind ai spend: %w", err)
	}
	return nil
}

// PriceAISpend lands the cost of one Lens request on the page it was bound to, EXACTLY ONCE.
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
func (s *Store) PriceAISpend(ctx context.Context, requestID string, costUSD float64, tokens int) (landed bool, err error) {
	if requestID == "" {
		return false, errors.New("page: PriceAISpend requires request_id")
	}
	var updated int
	qErr := s.pool.QueryRow(ctx, `
        WITH priced AS (
            UPDATE page_ai_spend_events
               SET cost_usd = $2, tokens = $3, priced_at = NOW()
             WHERE request_id = $1 AND cost_usd IS NULL
            RETURNING page_id, cost_usd
        ),
        rolled AS (
            UPDATE pages p
               SET own_ai_cost_usd = p.own_ai_cost_usd + priced.cost_usd,
                   updated_at = NOW()
              FROM priced
             WHERE p.id = priced.page_id
            RETURNING p.id
        ),
        bound AS (
            SELECT 1 FROM page_ai_spend_events WHERE request_id = $1
        )
        SELECT (SELECT count(*) FROM rolled), (SELECT count(*) FROM bound)`,
		requestID, costUSD, tokens,
	).Scan(&updated, new(int))
	if qErr != nil {
		return false, fmt.Errorf("page: price ai spend: %w", qErr)
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
