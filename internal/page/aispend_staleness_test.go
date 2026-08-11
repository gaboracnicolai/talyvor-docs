package page_test

import (
	"context"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// THE THIRD COPY OF THE STALENESS-CLOCK SEAM, AND THE ONLY ONE STILL LIVE.
//
// store.go's RecordView block records that a view bump which also set `updated_at = NOW()`
// reset the page's freshness clock, that it was measured on real Postgres, and that a
// recorded view not touching updated_at is now pinned by
// analytics.TestRecordedView_DoesNotResetTheStalenessClock_RealPG. That enumeration found
// the view writers. It did not find the COST writers, and there are two of them:
//
//	UpdateAICost   (the Track half)  UPDATE pages SET ai_cost_usd = $1 …
//	PriceAISpend   (the Lens half)   UPDATE pages SET own_ai_cost_usd = … , updated_at = NOW()
//
// One touches the clock and the other does not. MEASURED at 6f4aabb on real Postgres, one
// fixture, two pages each seeded 200 days past a 30-day TTL:
//
//	PriceAISpend($0.0031)  -> GetStalePages 1 -> 0 ; updated_at jumped 200 days to today
//	UpdateAICost($7.50)    -> GetStalePages 1 -> 1 ; updated_at unchanged at 200 days
//
// ⚠ THE OPERATION THAT PAID FOR IT NEED NOT HAVE CHANGED THE DOCUMENT. internal/ai binds a
// Lens request to a page for seven features, and `docs-ai-summarize` and `docs-ai-title` are
// READ-ONLY — the user asked for a summary, or for a title suggestion they may never accept.
// A three-tenth-of-a-cent summarize of a runbook nobody has touched since January silently
// takes it off the stale list, which is the one report whose entire job is to find
// documentation nobody is maintaining.
//
// ⚠ AND THE BUMP IS NEVER RIGHT, WHICH IS WHY THIS IS A FIX AND NOT A PRODUCT DECISION.
// Three independent reasons, any one sufficient:
//
//	· For the five features that DO change the page (write, grammar, shorter, longer,
//	  translate) the AI returns text to the editor; the user's save goes through Update,
//	  which bumps updated_at itself. The pricing bump is redundant there.
//	· For the two read-only features the page did not change at all. It is wrong there.
//	· PriceAISpend runs from a background sweep minutes-to-hours later, so the timestamp it
//	  writes is the moment of the BILLING SWEEP, not of the operation. Even under the reading
//	  most favourable to it, the value is wrong.
//
// ⚠ NOT CHANGED, AND IT IS NOT THE SAME THING: Store.Verify also sets updated_at = NOW().
// That is a human pressing "this is still accurate" — an attestation whose whole purpose is
// to reset the freshness clock, documented as such on the method. A background sweep is not.
func TestPricedAISpend_DoesNotResetTheStalenessClock_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "A runbook nobody has touched since January")
	store := page.NewStore(d.Pool)

	// 200 days past a 30-day TTL.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET stale_after_days = 30, updated_at = NOW() - INTERVAL '200 days' WHERE id = $1`,
		pageID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	onStaleList := func() bool {
		t.Helper()
		rows, err := store.GetStalePages(ctx, ws)
		if err != nil {
			t.Fatalf("GetStalePages: %v", err)
		}
		for _, p := range rows {
			if p.ID == pageID {
				return true
			}
		}
		return false
	}
	updatedAt := func() time.Time {
		t.Helper()
		var v time.Time
		if err := d.Pool.QueryRow(ctx, `SELECT updated_at FROM pages WHERE id = $1`, pageID).Scan(&v); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return v
	}
	ownCost := func() float64 {
		t.Helper()
		var v float64
		if err := d.Pool.QueryRow(ctx, `SELECT own_ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&v); err != nil {
			t.Fatalf("read own cost: %v", err)
		}
		return v
	}

	// [PREMISE-STALE] — the floor. "Still on the stale list" is satisfied by a list that was
	// never populated, by a broken predicate, and by a fixture whose seed did not take. Every
	// assertion below reads as a pass in those states unless this one runs first.
	if !onStaleList() {
		t.Fatalf("[PREMISE-STALE] the page is not on the stale list BEFORE any pricing — the "+
			"fixture did not take (updated_at=%v), so nothing below could distinguish a held "+
			"clock from an empty report.", updatedAt())
	}
	before := updatedAt()

	// A READ-ONLY AI operation: the user asked for a summary. internal/ai Engine.Summarize
	// binds exactly this. Nothing about the document changed.
	const reqID = "lens-req-summarize-0001"
	if err := store.BindAISpend(ctx, reqID, pageID, ws, "docs-ai-summarize"); err != nil {
		t.Fatalf("BindAISpend: %v", err)
	}
	if got := updatedAt(); !got.Equal(before) {
		t.Errorf("[CLOCK-HELD] BindAISpend moved updated_at %v -> %v; the binding is a record "+
			"that a request belonged to this page and must not touch the document.", before, got)
	}

	// ... and later the pricing sweep lands the cost.
	const cost = 0.0031
	landed, err := store.PriceAISpend(ctx, reqID, cost, 812)
	if err != nil {
		t.Fatalf("PriceAISpend: %v", err)
	}

	// [COST-LANDED] — the other floor, and the one that matters most. The obvious wrong way
	// to hold the clock is to stop pricing altogether, which would satisfy [CLOCK-HELD]
	// perfectly. The money must still move.
	if !landed {
		t.Errorf("[COST-LANDED] PriceAISpend reported landed=false — the cost must still be applied.")
	}
	if got := ownCost(); got != cost {
		t.Errorf("[COST-LANDED] own_ai_cost_usd = %v, want %v — holding the freshness clock "+
			"must not cost the ledger its write.", got, cost)
	}

	// [CLOCK-HELD] — the defect.
	if got := updatedAt(); !got.Equal(before) {
		t.Errorf("[CLOCK-HELD] pricing an AI request moved updated_at %v -> %v. GetStalePages "+
			"keys on that column, so a background billing sweep silently re-dated the document.",
			before, got)
	}
	if !onStaleList() {
		t.Errorf("[CLOCK-HELD] the page dropped off the stale list after a $%.4f read-only "+
			"summarize was priced. Nobody edited it; the freshness report is the one surface "+
			"whose job is to find exactly this document.", cost)
	}

	// [EDIT-STILL-MOVES-IT] — scope, not breakage. A real edit must still re-date the page and
	// take it off the list; a fix that froze updated_at generally would pass everything above.
	if _, err := store.Update(ctx, pageID, map[string]any{"content": `{"type":"doc"}`}); err != nil {
		t.Fatalf("[EDIT-STILL-MOVES-IT] Update: %v", err)
	}
	if got := updatedAt(); !got.After(before) {
		t.Errorf("[EDIT-STILL-MOVES-IT] a content save left updated_at at %v (was %v) — the "+
			"clock must still move for a genuine edit.", got, before)
	}
	if onStaleList() {
		t.Errorf("[EDIT-STILL-MOVES-IT] the page is still on the stale list after a content save.")
	}
}
