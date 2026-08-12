package lensintegration_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// A $0.00 ROW IS NOT A FREE COMPLETION, AND LENS SAYS SO ON THE WIRE.
//
// talyvor-lens writes a token_events row for a CACHE-SERVED or NODE-SERVED response with
// `cost_usd = 0` HARDCODED IN THE SQL (internal/alerts/alerts.go, insertCacheServeSQL — the same
// statement serves RecordCacheServe and RecordNodeServe) because zero is TALYVOR'S PROVIDER cost:
// no upstream API was called. What the requester owes for that serve lives in a different ledger
// (lxc_ledger / lens_token_ledger) in a different unit. Lens's own aggregations exclude those rows
// from provider spend for exactly this reason (internal/mcp/server.go cacheStatsSQL:
// `SUM(cost_usd) FILTER (WHERE serve_source NOT LIKE 'cache_hit%')`), and it emits `serve_source`
// on /v1/api/spend/by-request with the instruction, in the handler's own comment, to
// "render cache rows as 'served from cache', not 'free'".
//
// Docs's decoder did not carry the field. So a cache-served completion arrived as a $0.00 row that
// is byte-identical to an upstream call that genuinely cost nothing, was priced at $0.00, and —
// because the exactly-once guard is `cost_usd IS NULL` — can never be re-priced. The page then
// reports $0.00 of AI writing for work that happened, and PageView's panel is gated on
// `totalCost > 0`, so a document whose AI operations were ALL cache-served renders NO COST PANEL
// AT ALL: indistinguishable from a document nobody ever ran AI on.
//
// These guards do not decide what a cache serve should COST a page — that is a pricing question
// and it is reported, not merged. They hold the line before it: the fact Lens reported must not be
// thrown away at the decoder, because a zero nobody can explain is the shape this repo's own audit
// has now found three times.

// cacheRow is spendRow plus the serve_source Lens stamps on every by-request row. Kept beside
// spendRow (which omits the field, as an older Lens would) so the two shapes stay comparable.
func cacheRow(reqID string, cost float64, in, out int, serveSource string) map[string]any {
	r := spendRow(reqID, cost, in, out)
	r["serve_source"] = serveSource
	return r
}

// ledgerRow returns the whole page_ai_spend_events row as a map, minus the columns that differ
// between any two rows by construction — the key, and the two timestamps. Whatever is LEFT is what
// the ledger actually claims about a completion.
//
// ⚠ THE EXCLUSIONS ARE THE POINT AND THEY ARE NOT PADDING. priced_at is NOW() per statement and
// created_at is NOW() per bind, so leaving either in would make ANY two rows differ and the
// comparison below would pass without reading a single meaningful column — a guard that cannot
// fail. Everything else stays: page_id, workspace_id, operation, cost_usd, tokens, serve_source.
func ledgerRow(t *testing.T, d *testutil.DB, requestID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT row_to_json(e) FROM page_ai_spend_events e WHERE request_id = $1`, requestID,
	).Scan(&raw); err != nil {
		t.Fatalf("read ledger row %q: %v", requestID, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode ledger row %q: %v", requestID, err)
	}
	for _, k := range []string{"request_id", "created_at", "priced_at"} {
		delete(m, k)
	}
	return m
}

// ⚠ THE HEADLINE, AND IT ASSERTS AN INEQUALITY ON PURPOSE. Two completions on the SAME page, the
// same operation, the same tokens, both priced at $0.00 — one served upstream (genuinely free) and
// one served from Lens's cache (free to TALYVOR, not to the requester). If the ledger cannot tell
// them apart then nothing downstream can either, and the $0.00 the customer reads is unexplainable
// by construction rather than by measurement.
//
// This is the shape that survives the schema change: it names no new column, so it is a real red
// on the tree that has none rather than a query that errors because a column is missing.
func TestSweep_ACacheServeIsDistinguishableFromAGenuinelyFreeUpstreamCall(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")

	if err := st.BindAISpend(ctx, "req-upstream", pg, ws, "docs-ai-summarize"); err != nil {
		t.Fatalf("bind upstream: %v", err)
	}
	if err := st.BindAISpend(ctx, "req-cached", pg, ws, "docs-ai-summarize"); err != nil {
		t.Fatalf("bind cached: %v", err)
	}

	f := newFakeLensSpend(t)
	// Identical in every field Docs decoded before this change. The ONLY difference is the one
	// Lens ships to say what produced the bytes.
	f.setRows(ws,
		cacheRow("req-upstream", 0, 900, 120, "upstream"),
		cacheRow("req-cached", 0, 900, 120, "cache_hit_semantic"),
	)

	syncerFor(f, st).Sync(ctx)

	up := ledgerRow(t, d, "req-upstream")
	cached := ledgerRow(t, d, "req-cached")
	if reflectDeepEqualJSON(up, cached) {
		t.Fatalf("the ledger records a CACHE-SERVED completion and a genuinely free UPSTREAM one "+
			"identically: %v\n"+
			"Lens hardcodes cost_usd = 0 for a cache/node serve because that is TALYVOR'S provider "+
			"cost, and stamps serve_source so a consumer does not read it as 'free'. Dropping the "+
			"field at the decoder turns an unexplainable $0.00 into a customer-visible number — and "+
			"the exactly-once guard (`cost_usd IS NULL`) makes it permanent.", up)
	}
}

// reflectDeepEqualJSON compares two decoded rows. Written out rather than reflect.DeepEqual'd
// directly so the failure above can print the row it read.
func reflectDeepEqualJSON(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		aj, _ := json.Marshal(av)
		bj, _ := json.Marshal(bv)
		if string(aj) != string(bj) {
			return false
		}
	}
	return true
}

// The same fact, asserted directly on the column, so the failure names the value rather than an
// inequality. Redundant with the guard above ON THIS TREE and deliberately kept: the inequality
// holds the CLASS (any discriminator would satisfy it) and this holds the CONTRACT (it must be the
// value Lens reported, not a locally-invented flag).
func TestSweep_RecordsTheServeSourceLensReported(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")
	if err := st.BindAISpend(ctx, "req-c1", pg, ws, "docs-ai-write"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	f := newFakeLensSpend(t)
	f.setRows(ws, cacheRow("req-c1", 0, 100, 50, "cache_hit_pooled"))
	syncerFor(f, st).Sync(ctx)

	var got string
	if err := d.Pool.QueryRow(ctx,
		`SELECT serve_source FROM page_ai_spend_events WHERE request_id = 'req-c1'`).Scan(&got); err != nil {
		t.Fatalf("read serve_source: %v", err)
	}
	if got != "cache_hit_pooled" {
		t.Fatalf("serve_source = %q, want %q — the ledger must record what SERVED the completion, "+
			"which is the only thing that explains a $0.00 provider cost", got, "cache_hit_pooled")
	}
}

// ⚠ THREE STATES, NOT TWO. An older Lens (before migration 0100) sends no serve_source at all, and
// a row that arrived without one must not be recorded as 'upstream' — that would be Docs inventing
// the fact it exists to carry, and 'upstream' is the value that means "we did pay a provider".
// Empty is the honest third state: not reported.
func TestSweep_ARowWithNoServeSourceIsRecordedAsUnreportedNotUpstream(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")
	if err := st.BindAISpend(ctx, "req-old", pg, ws, "docs-ai-write"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	f := newFakeLensSpend(t)
	// spendRow, NOT cacheRow — the pre-0100 wire shape, with no serve_source key at all.
	f.setRows(ws, spendRow("req-old", 0.004, 100, 50))
	syncerFor(f, st).Sync(ctx)

	var got string
	if err := d.Pool.QueryRow(ctx,
		`SELECT serve_source FROM page_ai_spend_events WHERE request_id = 'req-old'`).Scan(&got); err != nil {
		t.Fatalf("read serve_source: %v", err)
	}
	if got != "" {
		t.Fatalf("serve_source = %q for a row Lens sent WITHOUT the field, want \"\" — recording a "+
			"value nobody reported is a fabricated fact on a money ledger", got)
	}
}

// MUST STAY GREEN — THE MONEY IS NOT WHAT CHANGED. An ordinary upstream row still prices exactly
// what it priced before, onto the same page. This guard passed before the change and is what makes
// the three above a recording change rather than a pricing one; if it ever reds, the merge that
// reds it changed a customer's number.
func TestSweep_RecordingTheServeSourceChangesNoMoney(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	st := page.NewStore(d.Pool)
	ws := d.Workspace(t)
	pg := d.Page(t, ws, d.Member(t, ws, "a@x.com"), "Runbook")
	if err := st.BindAISpend(ctx, "req-money", pg, ws, "docs-ai-write"); err != nil {
		t.Fatalf("bind: %v", err)
	}

	f := newFakeLensSpend(t)
	f.setRows(ws, cacheRow("req-money", 0.0125, 1000, 500, "upstream"))
	syncerFor(f, st).Sync(ctx)

	if got := ownCost(t, d, pg); got < 0.0125-1e-9 || got > 0.0125+1e-9 {
		t.Fatalf("own_ai_cost_usd = %v, want 0.0125 — recording serve_source must not move money", got)
	}
}
