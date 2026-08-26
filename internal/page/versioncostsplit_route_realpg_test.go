package page_test

// THE RECONCILIATION WAS GUARANTEED IN GO AND UNREACHABLE FROM THE PRODUCT.
//
// `Store.VersionCostSplit` landed with #190 carrying this argument, in its own docstring:
//
//	⚠ IT EXISTS BECAUSE A PER-REVISION FIGURE THAT DOES NOT ADD UP IS A LIE ABOUT MONEY THAT
//	LOOKS LIKE A FEATURE. Summing the ai_cost_usd column down a page's version history gives
//	Attributed and nothing else, and a reader has no way to tell that from the page's
//	own_ai_cost_usd.
//
// MEASURED at merged main `f1ad4db`, before this file existed: that function had ZERO production
// callers. `grep -rn VersionCostSplit internal/ frontend/src cmd/` returned its own definition,
// its own unit test, one comment in `internal/model/model.go` and one comment in
// `frontend/src/api/types.ts` — no handler, no route in `internal/page/handler.go`, no fetch. The
// guard that "requires them to reconcile" required it in a TEST. The product never asked.
//
// ⚠ AND THE CONSEQUENCE IS ON ONE SCREEN, WHICH IS WHY THIS IS A ROUTE AND NOT A REPORT.
// `frontend/src/pages/PageView.tsx` renders `<VersionHistory>` at :459 and, just below it, the
// AI-cost panel that prints `own_ai_cost_usd`. So a reader already sees BOTH numbers — the
// revision rows and the page total — and whenever a page carries pending or pre-0021 spend they
// do not add up, with nothing on screen saying why. #190's own fixture states the size of it:
// "version history shows $0.07 of a page that has spent $0.14".
//
// ⚠ WHAT THIS FILE ASSERTS IS THE SHIPPED ROUTE'S BYTES, deliberately, and it is the same reason
// `versioncost_realpg_test.go` gives for testing HTTP rather than a store return: a field that
// exists in Go and is dropped before the response is exactly as absent to a reader. Asserting
// `store.VersionCostSplit(...)` here would reproduce the defect this file exists to close.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// splitBody is only the fields this test is about, with explicit tags: decoding into a map would
// let a RENAMED key pass as a missing one, and every field here is money.
type splitBody struct {
	Attributed     *float64 `json:"attributed_usd"`
	Pending        *float64 `json:"pending_usd"`
	Unattributable *float64 `json:"unattributable_usd"`
	PageTotal      *float64 `json:"page_total_usd"`
}

// seedSplitPage builds the fixture #190 measured its windows against: one spend before the first
// save, two between the saves, one after the newest save. The amounts are distinct and non-round
// so no two attributions agree by accident, and the last one is PENDING by construction.
//
// ⚠ IT ALSO WRITES ONE PRE-0021 ROW WITH SQL, because `BindAISpend` can no longer produce one —
// it records the revision now. A fixture that could not express the legacy shape would leave the
// Unattributable bucket permanently untested, which is the bucket whose whole purpose is to admit
// that a fact was never captured.
func seedSplitPage(t *testing.T, d *testutil.DB, store *page.Store, ws, owner, pageID string) {
	t.Helper()
	ctx := context.Background()
	spend := func(reqID string, usd float64) {
		t.Helper()
		if err := store.BindAISpend(ctx, reqID, pageID, ws, "draft"); err != nil {
			t.Fatalf("bind %s: %v", reqID, err)
		}
		landed, err := store.PriceAISpend(ctx, reqID, usd, 100)
		if err != nil || !landed {
			t.Fatalf("price %s: landed=%v err=%v", reqID, landed, err)
		}
	}
	save := func(title, content string) {
		t.Helper()
		if _, err := store.Update(ctx, pageID, map[string]any{"title": title, "content": content}); err != nil {
			t.Fatalf("save %q: %v", title, err)
		}
	}

	spend("split-req-v1", 0.0400) // before save 1 → attributed to v1
	save("Protocol v1", `{"type":"doc","v":1}`)
	spend("split-req-v2-a", 0.0100) // between the saves → v2
	spend("split-req-v2-b", 0.0200) // between the saves → v2
	save("Protocol v2", `{"type":"doc","v":2}`)
	spend("split-req-pending", 0.0700) // after the newest save → PENDING

	// The legacy row: priced, on this page, with no revision — the shape every event written
	// before migration 0021 has, and the one no query can ever resolve.
	if _, err := d.Pool.Exec(ctx, `
        INSERT INTO page_ai_spend_events
            (request_id, page_id, workspace_id, operation, cost_usd, tokens, page_version, priced_at, created_at)
        VALUES ($1, $2, $3, 'draft', 0.0500, 100, NULL, now(), now())`,
		"split-req-legacy", pageID, ws,
	); err != nil {
		t.Fatalf("seed pre-0021 row: %v", err)
	}
	// own_ai_cost_usd is the column the split reconciles AGAINST, and the SQL insert above
	// bypasses PriceAISpend, which is what maintains it. Adding the legacy amount here keeps the
	// page total the sum of what was actually spent — otherwise the reconciliation would balance
	// only because one side was never told about the row.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET own_ai_cost_usd = own_ai_cost_usd + 0.0500 WHERE id = $1`, pageID,
	); err != nil {
		t.Fatalf("seed legacy page total: %v", err)
	}
}

func TestVersionCostSplit_IsSERVED_SoTheGapOnScreenIsExplained_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(d.Pool)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	pageID := d.Page(t, ws, owner, "Protocol")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	seedSplitPage(t, d, store, ws, owner, pageID)

	chain := createChain(d)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, getReq("/v1/spaces/"+spaceID+"/pages/"+pageID+"/version-cost", "owner@corp.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET version-cost: HTTP %d — %s\n\nThe split is computed in Go and served nowhere: "+
			"version history renders a per-revision cost and the page panel beside it renders "+
			"own_ai_cost_usd, and when they disagree the product has no way to say why.",
			rr.Code, rr.Body.String())
	}
	var got splitBody
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode split: %v — %s", err, rr.Body.String())
	}

	for _, f := range []struct {
		name string
		got  *float64
		want float64
		why  string
	}{
		{"attributed_usd", got.Attributed, 0.0300 + 0.0400, "the four requests bound before the newest save"},
		{"pending_usd", got.Pending, 0.0700, "bound after the newest save, so it names a revision that does not exist yet"},
		{"unattributable_usd", got.Unattributable, 0.0500, "the pre-0021 row, whose revision was never captured"},
		{"page_total_usd", got.PageTotal, 0.1900, "pages.own_ai_cost_usd, read rather than recomputed"},
	} {
		if f.got == nil {
			t.Errorf("%s is absent from the response — a bucket that is not served cannot explain "+
				"anything to a reader (%s)", f.name, f.why)
			continue
		}
		if !near(*f.got, f.want) {
			t.Errorf("%s = %.4f, want %.4f (%s)", f.name, *f.got, f.want, f.why)
		}
	}

	// ⚠ THE RECONCILIATION, ASSERTED ON THE BYTES. This is the property the store's own docstring
	// promises and that nothing checked at the boundary: if the handler drops a bucket, renames
	// one, or recomputes the total instead of reading the column, the three parts stop summing to
	// the whole and a reader is shown a number that does not add up.
	if got.Attributed != nil && got.Pending != nil && got.Unattributable != nil && got.PageTotal != nil {
		sum := *got.Attributed + *got.Pending + *got.Unattributable
		if !near(sum, *got.PageTotal) {
			t.Errorf("attributed %.4f + pending %.4f + unattributable %.4f = %.4f, but page_total_usd "+
				"is %.4f — the served split does not account for the page's whole spend, which is the "+
				"exact failure it exists to make unrepresentable",
				*got.Attributed, *got.Pending, *got.Unattributable, sum, *got.PageTotal)
		}
	}

	// ⚠ AND THE VISIBLE ROWS MUST EQUAL THE ATTRIBUTED BUCKET — the other half of the leak. The
	// two responses are what a reader actually has: if version history shows more than the split
	// calls attributed, the extra is money appearing on a revision the split says owns none.
	vrr := httptest.NewRecorder()
	chain.ServeHTTP(vrr, getReq("/v1/spaces/"+spaceID+"/pages/"+pageID+"/versions", "owner@corp.com"))
	if vrr.Code != http.StatusOK {
		t.Fatalf("GET versions: HTTP %d — %s", vrr.Code, vrr.Body.String())
	}
	var rows []versionRow
	if err := json.Unmarshal(vrr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode versions: %v — %s", err, vrr.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("precondition: %d revisions, want 2 — the sum below is only meaningful over a "+
			"history that has rows: %s", len(rows), vrr.Body.String())
	}
	var shown float64
	for _, v := range rows {
		if v.AICostUSD != nil {
			shown += *v.AICostUSD
		}
	}
	if got.Attributed != nil && !near(shown, *got.Attributed) {
		t.Errorf("version history shows %.4f across its rows but the served attributed_usd is %.4f "+
			"— the two surfaces a reader sees disagree about the same money", shown, *got.Attributed)
	}
}

// ⚠ THE SPLIT READS MONEY FOR A PAGE, so it carries the same workspace scope as every other
// per-page read here. `Store.VersionCostSplit` calls `assertInWorkspaces`, but a route that
// forgot to pass the caller's workspaces — or passed the page's own — would defeat it without
// changing a line of that function. This asserts the SHIPPED route refuses.
func TestVersionCostSplitRoute_RefusesAPageInAnotherWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(d.Pool)

	victimWS := d.Workspace(t)
	victim := d.Member(t, victimWS, "victim@corp.com")
	pageID := d.Page(t, victimWS, victim, "Acquisition")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	seedSplitPage(t, d, store, victimWS, victim, pageID)

	attackerWS := d.Workspace(t)
	d.Member(t, attackerWS, "attacker@corp.com")

	chain := createChain(d)

	// ⚠ THE PREMISE FIRST: the owner CAN read it. Without this, an assertion that the attacker
	// gets a non-200 would pass just as well against a route that is broken for everybody, or
	// against a page that was never seeded — the empty-set pass this repo keeps finding.
	orr := httptest.NewRecorder()
	chain.ServeHTTP(orr, getReq("/v1/spaces/"+spaceID+"/pages/"+pageID+"/version-cost", "victim@corp.com"))
	if orr.Code != http.StatusOK {
		t.Fatalf("premise: the page's OWN member cannot read the split either (HTTP %d) — the "+
			"refusal below would be evidence of nothing: %s", orr.Code, orr.Body.String())
	}
	var owned splitBody
	if err := json.Unmarshal(orr.Body.Bytes(), &owned); err != nil {
		t.Fatalf("decode owner split: %v", err)
	}
	if owned.PageTotal == nil || *owned.PageTotal <= 0 {
		t.Fatalf("premise: the owner's split reports no spend, so a leak would have nothing to "+
			"leak: %s", orr.Body.String())
	}

	arr := httptest.NewRecorder()
	chain.ServeHTTP(arr, getReq("/v1/spaces/"+spaceID+"/pages/"+pageID+"/version-cost", "attacker@corp.com"))
	if arr.Code == http.StatusOK {
		t.Fatalf("a member of another workspace read this page's AI spend split: HTTP 200 — %s",
			arr.Body.String())
	}
}

// ⚠ THE WHOLE AND ITS PARTS MUST BE TWO INDEPENDENT NUMBERS, AND THIS IS THE CASE THAT PROVES IT.
//
// `page_total_usd` is read from `pages.own_ai_cost_usd`; it is NOT the sum of the three buckets.
// The difference is invisible in a fixture where they happen to agree — and the fixture above is
// exactly that, by construction, because it balances on purpose. MEASURED: control C3 mutates the
// handler to serve `Attributed + Pending + Unattributable` as the total and the test above STAYS
// GREEN, because in a reconciling fixture the derived answer is the right one.
//
// So this case builds a page where they DISAGREE: money on `own_ai_cost_usd` that no priced event
// accounts for. That is not hypothetical — the split sums `page_ai_spend_events`, the column is
// maintained by `PriceAISpend`, and the two are separate records of the same money. Anything that
// removes an event row without touching the column (a retention sweep, a partial restore, a
// failed migration) produces exactly this shape, and it is the shape a reader most needs the
// screen to be honest about: the page spent MORE than anything can explain.
//
// A derived total would report the parts' sum and the discrepancy would vanish — the arithmetic
// balancing precisely because the money had gone missing. That is why the total is passed through.
func TestVersionCostSplit_ServesTheCOLUMNAsTheTotal_NotTheSumOfItsParts_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(d.Pool)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	pageID := d.Page(t, ws, owner, "Protocol")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("read seed space: %v", err)
	}
	seedSplitPage(t, d, store, ws, owner, pageID)

	// Money on the page that NO event row explains. The buckets can only ever sum to 0.19; the
	// column now says 0.24.
	const orphaned = 0.0500
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET own_ai_cost_usd = own_ai_cost_usd + $2 WHERE id = $1`, pageID, orphaned,
	); err != nil {
		t.Fatalf("seed orphaned page spend: %v", err)
	}

	chain := createChain(d)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, getReq("/v1/spaces/"+spaceID+"/pages/"+pageID+"/version-cost", "owner@corp.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET version-cost: HTTP %d — %s", rr.Code, rr.Body.String())
	}
	var got splitBody
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode split: %v — %s", err, rr.Body.String())
	}
	if got.Attributed == nil || got.Pending == nil || got.Unattributable == nil || got.PageTotal == nil {
		t.Fatalf("a bucket is absent, so the comparison below has nothing to compare: %s", rr.Body.String())
	}

	sum := *got.Attributed + *got.Pending + *got.Unattributable
	// The premise: this fixture must actually be one where the two differ, or the assertion after
	// it is satisfied by any implementation at all — the same emptiness that let C3 pass.
	if near(sum, *got.PageTotal) {
		t.Fatalf("premise: the buckets (%.4f) and the page total (%.4f) AGREE in this fixture, so "+
			"a total derived from the parts would be indistinguishable from one read from the "+
			"column and this case asserts nothing", sum, *got.PageTotal)
	}
	if !near(*got.PageTotal, 0.1900+orphaned) {
		t.Errorf("page_total_usd = %.4f, want %.4f — the served total is the SUM OF THE BUCKETS "+
			"(%.4f), not pages.own_ai_cost_usd. Derived that way the reconciliation can never "+
			"fail, and it would balance in exactly the case where money had gone missing from "+
			"the events that explain it",
			*got.PageTotal, 0.1900+orphaned, sum)
	}
}
