package lensintegration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// ⚠ THE PRICING SWEEP IS WHAT MAKES own_ai_cost_usd A REAL NUMBER.
//
// Without it the column exists, the bindings accumulate, and every page reports $0.00 forever — a
// value reported as measured that is structurally constant. This repo's own audit found exactly
// that shape twice (a cost-per-doc pinned to one workspace; page_type with no writer), and the
// last Docs review recorded "SyncPageCosts has zero tests" as a finding. Shipping a second
// unwritten column would repeat it.
//
// These run against real Postgres and a fake Lens, and assert the RECORDED ROW on the page.

// fakeLensSpend serves the two endpoints the sweep touches: the per-workspace token mint and
// /v1/api/spend/by-request. rowsFor lets a test vary the answer per workspace, including failing.
type fakeLensSpend struct {
	*httptest.Server
	mu      sync.Mutex
	rowsFor map[string][]map[string]any
	failFor map[string]bool
	pulls   map[string]int
}

func newFakeLensSpend(t *testing.T) *fakeLensSpend {
	t.Helper()
	f := &fakeLensSpend{
		rowsFor: map[string][]map[string]any{},
		failFor: map[string]bool{},
		pulls:   map[string]int{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			return
		}
		ws := r.Header.Get("X-Talyvor-Workspace")
		f.mu.Lock()
		f.pulls[ws]++
		fail := f.failFor[ws]
		rows := f.rowsFor[ws]
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows, "next_cursor": ""})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLensSpend) setRows(ws string, rows ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rowsFor[ws] = rows
}
func (f *fakeLensSpend) fail(ws string)   { f.mu.Lock(); f.failFor[ws] = true; f.mu.Unlock() }
func (f *fakeLensSpend) unfail(ws string) { f.mu.Lock(); f.failFor[ws] = false; f.mu.Unlock() }
func (f *fakeLensSpend) pullCount(ws string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pulls[ws]
}

func spendRow(reqID string, cost float64, in, out int) map[string]any {
	return map[string]any{
		"request_id": reqID, "feature": "docs-ai-write", "cost_usd": cost,
		"input_tokens": in, "output_tokens": out, "ts": time.Now().UTC().Format(time.RFC3339),
	}
}

func syncerFor(f *fakeLensSpend, st *page.Store) *lensintegration.PageCostSyncer {
	c := lensintegration.New(f.URL, "k1").WithTokenProvider(lenscreds.New(f.URL, "k1", lenscreds.Options{}))
	return lensintegration.NewPageCostSyncer(c, st, 2)
}

func ownCost(t *testing.T, d *testutil.DB, pageID string) float64 {
	t.Helper()
	var v float64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT own_ai_cost_usd FROM pages WHERE id = $1`, pageID).Scan(&v); err != nil {
		t.Fatalf("read own_ai_cost_usd: %v", err)
	}
	return v
}

// ⚠ THE HEADLINE. A bound request, priced by the sweep, lands on the page.
func TestSweep_PricesABoundRequestOntoItsPage(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")

	if err := st.BindAISpend(ctx, "req-1", pg, ws, "docs-ai-write"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	f := newFakeLensSpend(t)
	f.setRows(ws, spendRow("req-1", 0.0125, 1000, 500))

	syncerFor(f, st).Sync(ctx)

	if got := ownCost(t, d, pg); got < 0.0125-1e-9 || got > 0.0125+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v, want 0.0125. Without the sweep this column is a structural "+
			"zero: bindings accumulate and no cost ever reaches the page.", got)
	}
}

// ⚠ EXACTLY-ONCE ACROSS TICKS — AND NOTE WHICH PROTECTION THIS ACTUALLY EXERCISES.
//
// There are TWO independent defences against double-charging, and this test only reaches one of
// them. Measured, by deleting each in turn:
//
//	the sweep's UnpricedRequestIDs filter  → THIS test reds
//	the ledger's `cost_usd IS NULL` guard  → this test still PASSES; the store-level test
//	                                         TestOwnAICost_RepricingTheSameRequestCreditsNothing
//	                                         is what reds
//
// So do not read this as proof of the ledger guard. It proves the sweep does not re-submit priced
// requests; the backstop that makes a re-submission harmless is proven in internal/page. Both are
// needed — the filter is an optimisation that can be "simplified" away, and the guard is what
// keeps that from costing a customer money.
func TestSweep_RerunningDoesNotDoubleCharge(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")
	_ = st.BindAISpend(ctx, "req-dup", pg, ws, "docs-ai-write")

	f := newFakeLensSpend(t)
	f.setRows(ws, spendRow("req-dup", 0.02, 100, 100))
	s := syncerFor(f, st)

	s.Sync(ctx)
	s.Sync(ctx)
	s.Sync(ctx)

	if got := ownCost(t, d, pg); got < 0.02-1e-9 || got > 0.02+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v after THREE sweeps of one request, want 0.02 — the customer "+
			"was charged %.0fx for one completion", got, got/0.02)
	}
}

// ⚠ #51 MUST NOT REOPEN — EVERY WORKSPACE IS SWEPT. #51 fixed a cost loop that covered one
// workspace. This sweep takes its workspace list from the BINDINGS, so there is no configured
// workspace to pin; this proves it.
func TestSweep_CoversEveryWorkspaceWithWork(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)

	wsA, wsB := d.Workspace(t), d.Workspace(t)
	pgA := d.Page(t, wsA, d.Member(t, wsA, "a@x.com"), "A")
	pgB := d.Page(t, wsB, d.Member(t, wsB, "b@x.com"), "B")
	_ = st.BindAISpend(ctx, "req-a", pgA, wsA, "docs-ai-write")
	_ = st.BindAISpend(ctx, "req-b", pgB, wsB, "docs-ai-summarize")

	f := newFakeLensSpend(t)
	f.setRows(wsA, spendRow("req-a", 0.03, 10, 10))
	f.setRows(wsB, spendRow("req-b", 0.07, 10, 10))

	syncerFor(f, st).Sync(ctx)

	if got := ownCost(t, d, pgA); got < 0.03-1e-9 || got > 0.03+1e-9 {
		t.Errorf("workspace A page = %v, want 0.03", got)
	}
	if got := ownCost(t, d, pgB); got < 0.07-1e-9 || got > 0.07+1e-9 {
		t.Fatalf("workspace B page = %v, want 0.07. Only one workspace was swept — this is the #51 "+
			"single-workspace bug reopening on a new cost source.", got)
	}
}

// ⚠ ONE WORKSPACE FAILING MUST NOT STOP THE OTHERS, and the failed one keeps its bindings for the
// next tick rather than being written wrong or dropped.
func TestSweep_OneWorkspaceFailingDoesNotStarveTheRest(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)

	wsBad, wsGood := d.Workspace(t), d.Workspace(t)
	pgBad := d.Page(t, wsBad, d.Member(t, wsBad, "a@x.com"), "Bad")
	pgGood := d.Page(t, wsGood, d.Member(t, wsGood, "b@x.com"), "Good")
	_ = st.BindAISpend(ctx, "req-bad", pgBad, wsBad, "docs-ai-write")
	_ = st.BindAISpend(ctx, "req-good", pgGood, wsGood, "docs-ai-write")

	f := newFakeLensSpend(t)
	f.fail(wsBad)
	f.setRows(wsGood, spendRow("req-good", 0.05, 10, 10))
	s := syncerFor(f, st)

	s.Sync(ctx)

	if got := ownCost(t, d, pgGood); got < 0.05-1e-9 || got > 0.05+1e-9 {
		t.Fatalf("the healthy workspace was not priced (%v) because another workspace's pull failed", got)
	}
	if got := ownCost(t, d, pgBad); got != 0 {
		t.Fatalf("the failed workspace's page moved to %v — a pull that errored must write nothing", got)
	}

	// And the binding survives: the next tick, with Lens healthy, prices it.
	f.unfail(wsBad)
	f.setRows(wsBad, spendRow("req-bad", 0.09, 10, 10))
	s.Sync(ctx)
	if got := ownCost(t, d, pgBad); got < 0.09-1e-9 || got > 0.09+1e-9 {
		t.Fatalf("after recovery the binding priced to %v, want 0.09 — a failed pull must leave the "+
			"work for the next tick, not consume it", got)
	}
}

// A Lens workspace carries traffic from Track, the CLI and the extension too. Rows Docs never
// bound must not move any page.
func TestSweep_IgnoresRequestsItNeverBound(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")
	_ = st.BindAISpend(ctx, "req-ours", pg, ws, "docs-ai-write")

	f := newFakeLensSpend(t)
	f.setRows(ws,
		spendRow("req-ours", 0.01, 10, 10),
		spendRow("req-track-ENG-42", 5.00, 10, 10), // another tenant's traffic
		spendRow("req-cli", 3.00, 10, 10),
	)
	syncerFor(f, st).Sync(ctx)

	if got := ownCost(t, d, pg); got < 0.01-1e-9 || got > 0.01+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v, want 0.01 — the sweep priced traffic that was never bound "+
			"to a page", got)
	}
}

// ⚠ THE INVARIANT THE WHOLE DESIGN RESTS ON, PINNED.
//
// ai_cost_usd is RECOMPUTED AND OVERWRITTEN by the linked-issue sweep; own_ai_cost_usd is
// ACCUMULATED. If a future change ever routes the page's own cost into the overwritten column —
// or teaches the linked-issue sweep to write own_ai_cost_usd — the accumulated value is destroyed
// on the next tick, silently, and only for pages that had AI work. This test fails if that
// happens: it interleaves both writers and asserts each keeps its own number.
func TestSweep_LinkedIssueOverwriteNeverErasesAccumulatedOwnCost(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")

	_ = st.BindAISpend(ctx, "req-x", pg, ws, "docs-ai-write")
	f := newFakeLensSpend(t)
	f.setRows(ws, spendRow("req-x", 0.04, 10, 10))
	s := syncerFor(f, st)
	s.Sync(ctx)

	// The linked-issue sweep runs and overwrites its own column with a recomputed absolute.
	if err := st.UpdateAICost(ctx, pg, 2.50); err != nil {
		t.Fatalf("UpdateAICost: %v", err)
	}
	// More AI work arrives afterwards and must still accumulate.
	_ = st.BindAISpend(ctx, "req-y", pg, ws, "docs-ai-summarize")
	f.setRows(ws, spendRow("req-x", 0.04, 10, 10), spendRow("req-y", 0.06, 10, 10))
	s.Sync(ctx)

	own := ownCost(t, d, pg)
	if own < 0.10-1e-9 || own > 0.10+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v, want 0.10 (0.04 + 0.06). The linked-issue overwrite erased "+
			"accumulated AI cost — which is precisely why these are two columns.", own)
	}
	var linked float64
	_ = d.Pool.QueryRow(ctx, `SELECT ai_cost_usd FROM pages WHERE id = $1`, pg).Scan(&linked)
	if linked < 2.50-1e-9 || linked > 2.50+1e-9 {
		t.Errorf("ai_cost_usd = %v, want 2.50 — the linked-issue column must keep its own meaning", linked)
	}
}

// A workspace with nothing waiting must not be pulled at all. Not correctness, but the sweep runs
// every five minutes forever and a needless per-workspace round trip is a real cost at scale.
func TestSweep_DoesNotPullWorkspacesWithNothingWaiting(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")
	_ = st.BindAISpend(ctx, "req-once", pg, ws, "docs-ai-write")

	f := newFakeLensSpend(t)
	f.setRows(ws, spendRow("req-once", 0.01, 10, 10))
	s := syncerFor(f, st)

	s.Sync(ctx)
	after := f.pullCount(ws)
	s.Sync(ctx) // nothing unpriced remains
	if f.pullCount(ws) != after {
		t.Errorf("the sweep pulled a workspace with no unpriced bindings (%d → %d)", after, f.pullCount(ws))
	}
}
