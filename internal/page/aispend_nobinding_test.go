package page_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// TWO DIFFERENT FACTS THAT PriceAISpend REPORTED WITH THE SAME BYTES.
//
// `page.ErrNoBinding` is exported, documented in PriceAISpend's own `Returns:` block ("this
// request was never bound to a page"), and the statement already computes the count that decides
// it — a `bound` CTE selecting the binding row. That count was scanned into `new(int)` and
// thrown away, so the sentinel had ZERO producers anywhere in the repository: a whole-tree grep
// found the symbol in four places, all of them its own declaration or a comment about it.
//
// The consequence is not a wrong number, it is a missing distinction. Both of these returned
// `(false, nil)`:
//
//	a request the sweep already priced on an earlier tick — nothing left to do
//	a request Docs never bound to any page      — nothing was ever ours
//
// A reconciliation tool asking "did this Lens request belong to a document?" got silence and no
// way to tell the two apart; `errors.Is(err, page.ErrNoBinding)` was unreachable for every input.
//
// ⚠ WHAT THIS DOES NOT CHANGE, STATED SO NO ONE LOOKS FOR IT: no money moves differently. The
// roll-up SQL, the exactly-once `cost_usd IS NULL` predicate and the `landed` return are
// untouched — [MONEY-UNMOVED] below is the assertion that says so, not a claim in a comment.
//
// ⚠ WHY THE SENTINEL RATHER THAN THE SILENCE. `ai_spend.go` documents ErrNoBinding as "NOT an
// error condition, just a request that was not a page operation", and the test that used to sit
// in ai_spend_test.go read that as "must return a nil error". The two are different: the sentinel
// exists SO a caller can say "not ours, skip" without treating it as a fault, which is what the
// pricing sweep now does (see internal/lensintegration/nobinding_test.go). Its justification —
// "the sync would log an error on almost every row it sees" — is also measurably not the caller
// that exists: syncWorkspace pre-filters every pulled row against its OWN unpriced binding ids
// and never calls this method for a foreign request at all.
func TestOwnAICost_NeverBoundIsDistinguishableFromAlreadyPriced(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Runbook")
	s := store(t, d)

	if err := s.BindAISpend(ctx, "req-ours", pageID, ws, "docs-ai-summarize"); err != nil {
		t.Fatalf("BindAISpend: %v", err)
	}

	// [FIRST-PRICE-LANDS] — the floor. A fixture whose binding never priced would make every
	// assertion below true for the wrong reason: "already priced" would not be a real state.
	landed, err := s.PriceAISpend(ctx, "req-ours", 0.25, 1000)
	if err != nil {
		t.Fatalf("[FIRST-PRICE-LANDS] pricing a bound, unpriced request errored: %v", err)
	}
	if !landed {
		t.Fatal("[FIRST-PRICE-LANDS] pricing a bound, unpriced request reported landed=false — " +
			"the fixture never reached the state the rest of this test is about")
	}

	// [ALREADY-PRICED] — a re-pull. The sweep re-reads an overlapping window every tick by
	// design, so this is the ordinary path, and it must stay a silent no-op.
	relanded, reErr := s.PriceAISpend(ctx, "req-ours", 0.25, 1000)
	if reErr != nil {
		t.Errorf("[ALREADY-PRICED] re-pricing an already-priced request returned %v — a re-pull "+
			"is the ordinary path and must not report a fault", reErr)
	}
	if errors.Is(reErr, page.ErrNoBinding) {
		t.Errorf("[ALREADY-PRICED] an already-priced request reported ErrNoBinding — the binding " +
			"is right there; collapsing the two states the other way is not a fix")
	}
	if relanded {
		t.Error("[ALREADY-PRICED] re-pricing reported landed=true — exactly-once is the " +
			"`cost_usd IS NULL` predicate, not the caller's diligence")
	}

	before, bErr := s.OwnAICost(ctx, pageID)
	if bErr != nil {
		t.Fatalf("OwnAICost before the unbound call: %v", bErr)
	}

	// [NO-BINDING] — the headline. A request id Docs never bound to any page.
	strayLanded, strayErr := s.PriceAISpend(ctx, "req-not-ours", 9.99, 1)
	if !errors.Is(strayErr, page.ErrNoBinding) {
		t.Errorf("[NO-BINDING] pricing a request that was never bound returned err=%v, want "+
			"page.ErrNoBinding. PriceAISpend's own Returns: block promises this sentinel and the "+
			"statement already counts the binding row that decides it; while that count is "+
			"discarded, \"never ours\" and \"already priced\" are the same (false, nil) and "+
			"errors.Is(err, page.ErrNoBinding) is false for every possible input.", strayErr)
	}
	if strayLanded {
		t.Error("[NO-BINDING] an unbound request reported landed=true")
	}

	// [MONEY-UNMOVED] — a delta, not a total, so this assertion is about the unbound call alone
	// and cannot be reddened by a control that changes what the FIRST price was worth.
	after, aErr := s.OwnAICost(ctx, pageID)
	if aErr != nil {
		t.Fatalf("OwnAICost after the unbound call: %v", aErr)
	}
	if delta := after - before; delta < -1e-9 || delta > 1e-9 {
		t.Errorf("[MONEY-UNMOVED] own_ai_cost_usd moved by %v across a request that was never "+
			"bound to this page (before %v, after %v). Reporting the miss must not become a "+
			"second way to charge for one.", delta, before, after)
	}
}
