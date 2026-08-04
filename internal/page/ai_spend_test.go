package page_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// A DOCUMENT'S OWN AI COST LANDS ON THAT DOCUMENT.
//
// Before this, a page's ai_cost_usd was the sum of its linked Track ISSUES. AI work performed on
// the document — write, summarize, translate, title — was tagged in Lens by OPERATION
// ("docs-ai-write") and never by page, so it was attributable to nothing. These tests assert the
// recorded row, not a return value: the question is whether the money is ON the page.

func store(t *testing.T, d *testutil.DB) *page.Store {
	t.Helper()
	return page.NewStore(d.Pool)
}

// ⚠ THE HEADLINE. An AI operation on a page produces a cost attributable to THAT page.
func TestOwnAICost_AnAIOperationLandsOnItsPage(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := store(t, d)

	// Call time: Docs knows the page and Lens's request id. No cost yet — Docs must not guess one.
	if err := s.BindAISpend(ctx, "req-abc", pageID, ws, "docs-ai-summarize"); err != nil {
		t.Fatalf("BindAISpend: %v", err)
	}
	if got, err := s.OwnAICost(ctx, pageID); err != nil || got != 0 {
		t.Fatalf("own cost after an UNPRICED binding = %v (err %v), want 0 — a binding is not a charge", got, err)
	}

	// Sync time: Lens says what it cost.
	landed, err := s.PriceAISpend(ctx, "req-abc", 0.0042, 1500)
	if err != nil {
		t.Fatalf("PriceAISpend: %v", err)
	}
	if !landed {
		t.Fatal("PriceAISpend reported nothing landed for a bound, unpriced request")
	}

	got, err := s.OwnAICost(ctx, pageID)
	if err != nil {
		t.Fatalf("OwnAICost: %v", err)
	}
	if got < 0.0042-1e-9 || got > 0.0042+1e-9 {
		t.Fatalf("page own_ai_cost_usd = %v, want 0.0042. The cost of AI work on this document did "+
			"not land on this document — which is the entire finding.", got)
	}
}

// ⚠ THE ZERO CASE, asserted rather than assumed. A page with no AI work and no linked issues
// reads zero on BOTH columns — so a reader can tell "nothing happened" from "we lost track".
func TestOwnAICost_UntouchedPageReadsZeroOnBothColumns(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Untouched")
	s := store(t, d)

	own, err := s.OwnAICost(ctx, pageID)
	if err != nil {
		t.Fatalf("OwnAICost: %v", err)
	}
	if own != 0 {
		t.Errorf("own_ai_cost_usd = %v on a page with no AI work, want 0", own)
	}
	var linked float64
	if err := d.Pool.QueryRow(ctx, `SELECT ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&linked); err != nil {
		t.Fatalf("read ai_cost_usd: %v", err)
	}
	if linked != 0 {
		t.Errorf("ai_cost_usd = %v on a page with no linked issues, want 0", linked)
	}
}

// ⚠ EXACTLY-ONCE, WHICH IS NOT OPTIONAL. The sync re-reads an overlapping window every tick by
// design, so the same request arrives repeatedly. Charging twice for one completion is the money
// bug this ledger exists to prevent.
func TestOwnAICost_RepricingTheSameRequestCreditsNothing(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := store(t, d)

	if err := s.BindAISpend(ctx, "req-dup", pageID, ws, "docs-ai-write"); err != nil {
		t.Fatalf("BindAISpend: %v", err)
	}
	if _, err := s.PriceAISpend(ctx, "req-dup", 0.01, 100); err != nil {
		t.Fatalf("first price: %v", err)
	}
	landed, err := s.PriceAISpend(ctx, "req-dup", 0.01, 100)
	if err != nil {
		t.Fatalf("second price: %v", err)
	}
	if landed {
		t.Error("the second pricing of the same request reported landed=true")
	}

	got, _ := s.OwnAICost(ctx, pageID)
	if got < 0.01-1e-9 || got > 0.01+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v after pricing one request TWICE, want 0.01 — a re-pull "+
			"double-charged the customer", got)
	}
}

// A re-bind of the same request id must not duplicate the row either — a retried handler is
// ordinary, and it must not create a second chargeable binding.
func TestOwnAICost_RebindingIsIdempotent(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := store(t, d)

	for i := 0; i < 3; i++ {
		if err := s.BindAISpend(ctx, "req-retry", pageID, ws, "docs-ai-title"); err != nil {
			t.Fatalf("bind %d: %v", i, err)
		}
	}
	if _, err := s.PriceAISpend(ctx, "req-retry", 0.02, 50); err != nil {
		t.Fatalf("price: %v", err)
	}
	got, _ := s.OwnAICost(ctx, pageID)
	if got < 0.02-1e-9 || got > 0.02+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v after three binds and one price, want 0.02", got)
	}
}

// ⚠ THE TWO COLUMNS MUST NOT BLEED. #51 made the linked-issue sweep overwrite ai_cost_usd with a
// recomputed absolute; if a page's own cost lived in that column the next sweep would erase it.
// This asserts the separation directly: writing the linked-issue total leaves own_ai_cost_usd
// untouched, which is the mechanical reason the two numbers cannot share a column.
func TestOwnAICost_LinkedIssueSweepDoesNotEraseTheOwnCost(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := store(t, d)

	if err := s.BindAISpend(ctx, "req-sep", pageID, ws, "docs-ai-write"); err != nil {
		t.Fatalf("BindAISpend: %v", err)
	}
	if _, err := s.PriceAISpend(ctx, "req-sep", 0.05, 200); err != nil {
		t.Fatalf("PriceAISpend: %v", err)
	}

	// The linked-issue sweep runs and overwrites ai_cost_usd with its recomputed absolute.
	if err := s.UpdateAICost(ctx, pageID, 1.23); err != nil {
		t.Fatalf("UpdateAICost: %v", err)
	}

	own, _ := s.OwnAICost(ctx, pageID)
	if own < 0.05-1e-9 || own > 0.05+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v after the linked-issue sweep, want 0.05. The sweep overwrote "+
			"the page's own AI cost — which is exactly what putting both numbers in one column does.", own)
	}
	var linked float64
	_ = d.Pool.QueryRow(ctx, `SELECT ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&linked)
	if linked < 1.23-1e-9 || linked > 1.23+1e-9 {
		t.Errorf("ai_cost_usd = %v, want 1.23 — the linked-issue column must keep its own meaning", linked)
	}
}

// A request that was never bound is NOT an error. The sync pulls every request in the Lens
// workspace and most belong to other tenants of it; treating a miss as a failure would make the
// sync log an error on almost every row it sees.
func TestOwnAICost_PricingAnUnboundRequestIsNotAnError(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	s := store(t, d)

	landed, err := s.PriceAISpend(ctx, "req-not-ours", 9.99, 1)
	if err != nil {
		t.Fatalf("pricing an unbound request returned an error: %v — a request that was not a page "+
			"operation is the common case, not a fault", err)
	}
	if landed {
		t.Error("an unbound request reported landed=true")
	}
}
