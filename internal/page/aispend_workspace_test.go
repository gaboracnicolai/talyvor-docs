package page_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// THE LEDGER ROW'S TWO IDS MUST AGREE ABOUT TENANCY.
//
// page_ai_spend_events carries a page_id and a workspace_id. workspace_id is the workspace that
// PAID Lens for the completion; page_id is what PriceAISpend will roll the money onto, with
// `WHERE p.id = priced.page_id` and no workspace predicate at all. The two arrive from different
// places — the URL path and the request body — and are authorized by two different gates, so
// until BindAISpend nothing in the process holds both at once.
//
// THE FOREIGN KEY CANNOT SEE THIS. `page_id REFERENCES pages(id)` is satisfied by any real page in
// any workspace; the row it rejects is one naming a page that does not exist, which is not the
// failure mode. A row whose page exists in ANOTHER tenant is exactly as valid to the schema and
// exactly as false to a customer.
//
// The HTTP gate that stops this reaching the store is in internal/ai (attributable, measured in
// billingworkspace_realpg_test.go). This file is the other half: the invariant stated where the
// row is written, so a future caller that does not go through that handler cannot produce one.
// The two are not redundant — the handler's assertions are about a 404 and an uncalled Lens, and
// no assertion in this file can be satisfied by them.

// TestBindAISpend_RefusesAPageOutsideTheBilledWorkspace is the headline: the row is refused, and
// refused LOUDLY rather than by writing nothing and reporting success.
func TestBindAISpend_RefusesAPageOutsideTheBilledWorkspace(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@a.example")
	bob := d.Member(t, wsB, "bob@b.example")
	pageB := d.Page(t, wsB, bob, "B Roadmap")
	pageA := d.Page(t, wsA, alice, "A Draft")
	s := page.NewStore(d.Pool)

	rows := func(t *testing.T, pageID string) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM page_ai_spend_events WHERE page_id = $1`, pageID).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		return n
	}

	// ── PREMISE, asserted before any behaviour: the two pages really are in different
	// workspaces and the ledger starts empty. Without this "no row" could mean "no seed".
	var owner string
	if err := d.Pool.QueryRow(ctx, `SELECT workspace_id FROM pages WHERE id = $1`, pageB).Scan(&owner); err != nil {
		t.Fatalf("[P-PREMISE] read pageB workspace: %v", err)
	}
	if owner != wsB || wsA == wsB {
		t.Fatalf("[P-PREMISE] fixture cannot express a mismatch: pageB owner=%s wsB=%s wsA=%s", owner, wsB, wsA)
	}
	if n := rows(t, pageB); n != 0 {
		t.Fatalf("[P-PREMISE] pageB starts with %d ledger rows, want 0", n)
	}

	// ── THE HONEST BINDING, FIRST AND UNCONDITIONALLY. A fix that refuses everything passes
	// every refusal assertion below and fails here.
	if err := s.BindAISpend(ctx, "req-honest", pageA, wsA, "docs-ai-write"); err != nil {
		t.Fatalf("[P-HONEST] binding a page to its OWN workspace failed: %v", err)
	}
	if n := rows(t, pageA); n != 1 {
		t.Fatalf("[P-HONEST] pageA has %d ledger rows after its own binding, want 1", n)
	}

	// ── THE MISMATCH.
	err := s.BindAISpend(ctx, "req-cross", pageB, wsA, "docs-ai-write")
	if !errors.Is(err, page.ErrPageWorkspaceMismatch) {
		t.Errorf("[P-REFUSED] BindAISpend(page in %s, billed to %s) = %v, want ErrPageWorkspaceMismatch — "+
			"the row would say workspace %s paid for work on a document in workspace %s, and "+
			"PriceAISpend rolls it onto that document with no workspace predicate",
			wsB, wsA, err, wsA, wsB)
	}
	if n := rows(t, pageB); n != 0 {
		t.Errorf("[P-NOROW] pageB has %d ledger rows after a wsA-billed binding, want 0", n)
	}

	// ── AND THE MONEY CANNOT FOLLOW. Pricing the request that was refused must credit nothing:
	// an error the caller ignored (Engine.run only logs it) must still leave the ledger unable
	// to move money. This is the assertion the refusal exists FOR — the error alone proves a
	// return value, this proves the customer-visible number.
	landed, pErr := s.PriceAISpend(ctx, "req-cross", 12.34, 1000)
	if pErr != nil {
		t.Fatalf("PriceAISpend(req-cross): %v", pErr)
	}
	if landed {
		t.Errorf("[P-NOMONEY] PriceAISpend reported landed=true for a refused binding")
	}
	var own float64
	if err := d.Pool.QueryRow(ctx, `SELECT own_ai_cost_usd FROM pages WHERE id = $1`, pageB).Scan(&own); err != nil {
		t.Fatalf("read own_ai_cost_usd: %v", err)
	}
	if own != 0 {
		t.Errorf("[P-NOMONEY] pageB.own_ai_cost_usd = %v, want 0 — workspace %s paid for this", own, wsA)
	}
}

// TestBindAISpend_MismatchIsDistinguishableFromAReBind pins the one thing a RowsAffected-shaped
// implementation cannot express. `ON CONFLICT (request_id) DO NOTHING` writes zero rows for an
// ordinary re-bind, which is SUCCESS; a mismatch also writes zero rows, and is a refusal. An
// implementation that read the row count would return the same answer for both, and the loud half
// would go silent.
func TestBindAISpend_MismatchIsDistinguishableFromAReBind(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@a.example")
	bob := d.Member(t, wsB, "bob@b.example")
	pageA := d.Page(t, wsA, alice, "A Draft")
	pageB := d.Page(t, wsB, bob, "B Roadmap")
	s := page.NewStore(d.Pool)

	if err := s.BindAISpend(ctx, "req-1", pageA, wsA, "docs-ai-write"); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	// The re-bind writes NO row and must still report success — the sync re-reads an overlapping
	// window by design.
	if err := s.BindAISpend(ctx, "req-1", pageA, wsA, "docs-ai-write"); err != nil {
		t.Errorf("[P-REBIND] re-binding the same request returned %v, want nil — a retried handler "+
			"or a duplicate request id must record once and report success", err)
	}
	// The mismatch also writes NO row and must NOT report success.
	if err := s.BindAISpend(ctx, "req-2", pageB, wsA, "docs-ai-write"); !errors.Is(err, page.ErrPageWorkspaceMismatch) {
		t.Errorf("[P-DISTINCT] a cross-workspace bind returned %v, want ErrPageWorkspaceMismatch — "+
			"it writes zero rows for the same reason the re-bind above does, so any "+
			"implementation that reads the row count reports both as the same outcome", err)
	}
}
