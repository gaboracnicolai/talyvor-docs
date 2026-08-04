package lensintegration

import (
	"context"
	"log/slog"
)

// THE PRICING SWEEP — turns page↔request bindings into money on the page.
//
// ⚠ HOW THIS RELATES TO #51's INCOMPLETE-TOTAL RULE, stated explicitly because a new cost source
// is exactly the kind of change that reopens a closed hole.
//
// #51 fixed two things in the LINKED-ISSUE sweep: it now enumerates every workspace, and it
// refuses to write a total it could not compute completely (a partial sum would overwrite a
// complete one, because that sweep writes an ABSOLUTE recomputed value).
//
// Neither can be reopened here, and not by discipline — by shape:
//
//  1. DIFFERENT COLUMN. This sweep never touches pages.ai_cost_usd. The linked-issue sweep still
//     owns it, still recomputes it, and still skips the write when incomplete.
//  2. NOTHING IS EVER OVERWRITTEN. own_ai_cost_usd is incremented per ledger row, exactly-once,
//     guarded by `cost_usd IS NULL` inside the UPDATE. There is no total to be partial: a short
//     pull prices FEWER requests, each correctly, and the next tick prices the rest. The failure
//     mode #51 closed has no analogue here because it belongs to recomputed absolutes.
//  3. MULTI-WORKSPACE BY CONSTRUCTION. The workspaces come from the bindings themselves
//     (UnpricedWorkspaces), not from a pinned config value — so this sweep cannot regress to the
//     single-workspace bug #51 fixed, because there is no single workspace anywhere in it.
//
// And a pull that fails for one workspace does not stop the others: each is independent, and a
// workspace we could not reach keeps its unpriced bindings for the next tick.

// PageSpendStore is the slice of the page store this sweep needs.
type PageSpendStore interface {
	UnpricedWorkspaces(ctx context.Context, limit int) ([]string, error)
	UnpricedRequestIDs(ctx context.Context, workspaceID string, limit int) ([]string, error)
	PriceAISpend(ctx context.Context, requestID string, costUSD float64, tokens int) (bool, error)
}

// PageCostSyncer prices page↔request bindings from Lens.
type PageCostSyncer struct {
	client *Client
	pages  PageSpendStore
	days   int
}

// NewPageCostSyncer wires the sweep. days bounds how far back a pull reaches; bindings older than
// that stay unpriced rather than being priced wrongly, and are visible as such in the ledger.
func NewPageCostSyncer(c *Client, pages PageSpendStore, days int) *PageCostSyncer {
	if days <= 0 {
		days = 2
	}
	return &PageCostSyncer{client: c, pages: pages, days: days}
}

// Sync prices what it can, for every workspace that has anything waiting.
func (s *PageCostSyncer) Sync(ctx context.Context) {
	if s == nil || s.client == nil || s.pages == nil || !s.client.IsConfigured() {
		return
	}
	wsIDs, err := s.pages.UnpricedWorkspaces(ctx, 200)
	if err != nil {
		slog.Warn("lensintegration: page cost sync — enumerate workspaces", slog.String("err", err.Error()))
		return
	}
	for _, wsID := range wsIDs {
		s.syncWorkspace(ctx, wsID)
	}
}

func (s *PageCostSyncer) syncWorkspace(ctx context.Context, wsID string) {
	want, err := s.pages.UnpricedRequestIDs(ctx, wsID, 500)
	if err != nil {
		slog.Warn("lensintegration: page cost sync — unpriced ids",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		return
	}
	if len(want) == 0 {
		return
	}
	rows, err := s.client.SpendByRequest(ctx, wsID, s.days)
	if err != nil {
		// A workspace we could not read keeps its bindings. Nothing is written, so nothing is
		// wrong — the next tick retries. Logged because a pull that fails forever is a real
		// problem that would otherwise look like "no AI work happened".
		slog.Warn("lensintegration: page cost sync — spend pull failed; bindings remain unpriced",
			slog.String("workspace_id", wsID), slog.String("err", err.Error()))
		return
	}

	// Only the request ids we bound are ours. A Lens workspace carries traffic from Track, the
	// CLI and the extension too, and pricing a row we never bound is impossible anyway
	// (PriceAISpend matches on the binding) — this just avoids the pointless round trips.
	ours := make(map[string]struct{}, len(want))
	for _, id := range want {
		ours[id] = struct{}{}
	}

	var landed int
	var landedUSD float64
	for _, r := range rows {
		if _, mine := ours[r.RequestID]; !mine {
			continue
		}
		ok, pErr := s.pages.PriceAISpend(ctx, r.RequestID, r.CostUSD, r.InputTokens+r.OutputTokens)
		if pErr != nil {
			slog.Warn("lensintegration: page cost sync — price failed",
				slog.String("workspace_id", wsID),
				slog.String("request_id", r.RequestID),
				slog.String("err", pErr.Error()))
			continue
		}
		if ok {
			landed++
			landedUSD += r.CostUSD
		}
	}
	slog.Info("lensintegration: page cost sync",
		slog.String("workspace_id", wsID),
		slog.Int("bindings_waiting", len(want)),
		slog.Int("priced", landed),
		slog.Float64("priced_usd", landedUSD),
		slog.Int("still_unpriced", len(want)-landed))
}
