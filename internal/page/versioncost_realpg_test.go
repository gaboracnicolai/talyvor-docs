package page_test

// COST PER REVISION IN VERSION HISTORY — THE SITE SELLS IT AND NOTHING COMPUTES IT.
//
// talyvor.higgsfield.app/products/docs lists, present tense, under "The detail":
//
//	+ PER-PAGE RUNNING AI COST
//	+ COST PER REVISION IN VERSION HISTORY      ← this one
//	+ PAGE, SPACE AND ORG ROLLUPS
//
// Measured before writing a line: `page_versions` (0002_pages.sql:47) has id, page_id, version,
// title, content, created_by, created_at and — since 0015 — workspace_id. No cost column.
// `model.PageVersion` and the SPA's `PageVersion` both carry the same eight fields and no cost.
// `frontend/src/components/VersionHistory.tsx` contains the string "cost" ZERO times. Money lives
// on `pages.own_ai_cost_usd`, rolled up by page and never by revision.
//
// ⚠ AND THE JOIN KEY WAS NOT CAPTURED, WHICH IS WHY THIS IS A BUILD AND NOT A QUERY.
// `page_ai_spend_events` (0018) records request_id, page_id, workspace_id, operation, cost_usd,
// tokens, priced_at, created_at. It knows WHICH PAGE a completion was for and has never known
// WHICH REVISION. A per-revision figure derived by comparing timestamps would be a guess about
// money, so the revision is recorded at bind time instead — the one moment the writer knows it.
//
// ⚠ WHAT THIS TEST ASSERTS IS THE SHIPPED ROUTE'S BYTES, not a store return value. The claim on
// the site is about what version history SHOWS, and a field that exists in Go and is dropped
// before the response is exactly as absent to a reader.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func getReq(path, email string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Gateway-Auth", testGatewaySecret)
	r.Header.Set("X-User-Email", email)
	return r
}

// versionRow is only the fields this test is about; decoding into a map would let a renamed key
// pass as a missing one.
type versionRow struct {
	Version   int      `json:"version"`
	Title     string   `json:"title"`
	AICostUSD *float64 `json:"ai_cost_usd"`
}

func TestVersionHistory_ReportsTheAICostAttributedToEachRevision_RealPG(t *testing.T) {
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

	// ⚠ THE ORDER IS THE TEST. Each spend is bound while a DIFFERENT revision is the live one, and
	// the amounts are distinct and non-round so a harness that attributed every event to the newest
	// revision, or to the oldest, or summed them all onto one, gives a different answer to each.
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

	// ⚠ THE WINDOWS, AND THEY WERE MEASURED RATHER THAN ASSUMED. Probed on real Postgres: a page
	// seeded and saved twice leaves page_versions v1 = the state AFTER save 1 and v2 = the state
	// AFTER save 2, with the seed state in NO row. So the work that PRODUCED revision N is the
	// spend bound BEFORE save N. The first draft of this test asserted the opposite — that a
	// revision owns the spend that came after it — and it is the reading a person gets from the
	// column name alone. Both readings put a plausible number on every row; only the probe
	// separates them.
	spend("req-producing-v1", 0.0400) // before save 1 → this is what v1 cost to write
	save("Protocol v1", `{"type":"doc","v":1}`)
	spend("req-producing-v2-a", 0.0100) // after save 1, before save 2 → v2
	spend("req-producing-v2-b", 0.0200) // after save 1, before save 2 → v2
	save("Protocol v2", `{"type":"doc","v":2}`)
	spend("req-producing-v3", 0.0700) // after the newest save → PENDING, v3 does not exist yet

	chain := createChain(d)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, getReq("/v1/spaces/"+spaceID+"/pages/"+pageID+"/versions", "owner@corp.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET versions: HTTP %d — %s", rr.Code, rr.Body.String())
	}
	var got []versionRow
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode versions: %v — %s", err, rr.Body.String())
	}
	byVersion := map[int]versionRow{}
	for _, v := range got {
		byVersion[v.Version] = v
	}
	if len(byVersion) != 2 {
		t.Fatalf("precondition: %d versions in history, want 2 — every assertion below is about "+
			"which revision a cost landed on and needs two to distinguish: %s", len(byVersion), rr.Body.String())
	}

	for _, c := range []struct {
		version int
		want    float64
		what    string
	}{
		{1, 0.0400, "the one request bound before save 1, which is the work v1 records"},
		{2, 0.0300, "the two requests bound between save 1 and save 2, which is the work v2 records"},
	} {
		row, ok := byVersion[c.version]
		if !ok {
			t.Errorf("v%d missing from version history", c.version)
			continue
		}
		if row.AICostUSD == nil {
			t.Errorf("v%d reports NO ai_cost_usd — the site sells \"COST PER REVISION IN VERSION "+
				"HISTORY\" and version history carries no cost field at all; want %.4f from %s",
				c.version, c.want, c.what)
			continue
		}
		if diff := *row.AICostUSD - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("v%d ai_cost_usd = %.4f, want %.4f (%s)", c.version, *row.AICostUSD, c.want, c.what)
		}
	}

	// ⚠ THE RECONCILIATION IS THE POINT, NOT AN EXTRA. Version history here shows $0.07 of a page
	// that has spent $0.14. A per-revision column that does not add up to the page's own figure is
	// a lie about money shaped like a feature, and the two assertions below make a leak
	// unrepresentable: the visible rows must sum to exactly the Attributed bucket, and the three
	// buckets must sum to exactly pages.own_ai_cost_usd. A pool that is silently dropped fails the
	// second; a pool that is silently shown fails the first.
	split, err := store.VersionCostSplit(ctx, pageID, []string{ws})
	if err != nil {
		t.Fatalf("VersionCostSplit: %v", err)
	}
	var shown float64
	for _, v := range got {
		if v.AICostUSD != nil {
			shown += *v.AICostUSD
		}
	}
	if near(shown, split.Attributed) == false {
		t.Errorf("version history shows %.4f but the Attributed bucket is %.4f — money is "+
			"attributed to a revision the history does not display", shown, split.Attributed)
	}
	if !near(split.Pending, 0.0700) {
		t.Errorf("Pending = %.4f, want 0.0700 — the request bound after the newest save names v3, "+
			"which does not exist yet; it must be reported as pending, not dropped and not shown "+
			"on an existing revision", split.Pending)
	}
	if !near(split.Unattributable, 0) {
		t.Errorf("Unattributable = %.4f, want 0 — every event in this test was written after "+
			"migration 0021 and carries a revision", split.Unattributable)
	}
	if !near(split.PageTotal, 0.1400) {
		t.Errorf("PageTotal = %.4f, want 0.1400 (pages.own_ai_cost_usd)", split.PageTotal)
	}
	if sum := split.Attributed + split.Pending + split.Unattributable; !near(sum, split.PageTotal) {
		t.Errorf("the three buckets sum to %.4f but the page's own_ai_cost_usd is %.4f — %.4f of "+
			"priced spend is in no bucket at all", sum, split.PageTotal, split.PageTotal-sum)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// ⚠ THE LEGACY ROW IS THE CASE THAT DECIDES WHETHER THIS FEATURE LIES ABOUT MONEY.
//
// Every page_ai_spend_events row written before migration 0021 has page_version NULL, and no query
// can recover the revision — the fact was never captured. There are exactly three ways to render
// such a page and two of them are false: fold the money onto a revision (a specific claim nobody
// measured), or drop it (version history then shows less than the page spent, with nothing saying
// so). This asserts the third: it is reported, as its own figure, and it reaches no revision row.
//
// The row is inserted with SQL on purpose. BindAISpend can no longer produce one — that is the
// fix — so the only way to hold the pre-0021 shape still is to write it, and a fixture that could
// not express the legacy state would leave this bucket permanently untested.
func TestVersionHistory_PreMigrationSpendIsReportedUnattributable_NotFoldedOntoARevision_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(d.Pool)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	pageID := d.Page(t, ws, owner, "Legacy")

	if _, err := store.Update(ctx, pageID, map[string]any{"title": "L1", "content": `{"b":1}`}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A row exactly as 0018 wrote them: no page_version.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO page_ai_spend_events (request_id, page_id, workspace_id, operation)
		 VALUES ($1, $2, $3, 'draft')`, "req-legacy", pageID, ws); err != nil {
		t.Fatalf("seed legacy binding: %v", err)
	}
	var pv *int
	if err := d.Pool.QueryRow(ctx, `SELECT page_version FROM page_ai_spend_events WHERE request_id='req-legacy'`).Scan(&pv); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if pv != nil {
		t.Fatalf("precondition: the seeded legacy row carries page_version=%d — it must be NULL or "+
			"this test is about a row the fix already handles", *pv)
	}
	landed, err := store.PriceAISpend(ctx, "req-legacy", 0.0900, 100)
	if err != nil || !landed {
		t.Fatalf("price legacy: landed=%v err=%v", landed, err)
	}

	versions, err := store.GetVersions(ctx, pageID, []string{ws})
	if err != nil {
		t.Fatalf("GetVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("precondition: %d versions, want 1", len(versions))
	}
	if got := versions[0].AICostUSD; !near(got, 0) {
		t.Errorf("v1 ai_cost_usd = %.4f, want 0 — a completion whose revision was never recorded "+
			"must not be attributed to one; %.4f is a claim about which revision that money paid "+
			"for, and nobody measured it", got, got)
	}

	split, err := store.VersionCostSplit(ctx, pageID, []string{ws})
	if err != nil {
		t.Fatalf("VersionCostSplit: %v", err)
	}
	if !near(split.Unattributable, 0.0900) {
		t.Errorf("Unattributable = %.4f, want 0.0900 — the legacy row is neither on a revision nor "+
			"reported, so version history is quietly $0.09 short of what the page spent",
			split.Unattributable)
	}
	if !near(split.Attributed, 0) || !near(split.Pending, 0) {
		t.Errorf("Attributed=%.4f Pending=%.4f, want 0 and 0 — the legacy row belongs to neither",
			split.Attributed, split.Pending)
	}
	if sum := split.Attributed + split.Pending + split.Unattributable; !near(sum, split.PageTotal) {
		t.Errorf("buckets sum to %.4f, page own_ai_cost_usd is %.4f", sum, split.PageTotal)
	}
}

// VersionCostSplit reads money for a page, so it carries the same workspace scope as every other
// read in this store rather than trusting its caller.
func TestVersionCostSplit_RefusesAPageInAnotherWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	store := page.NewStore(d.Pool)

	victimWS := d.Workspace(t)
	victim := d.Member(t, victimWS, "victim@corp.com")
	pageID := d.Page(t, victimWS, victim, "Theirs")
	if err := store.BindAISpend(ctx, "req-x", pageID, victimWS, "draft"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, err := store.PriceAISpend(ctx, "req-x", 0.5000, 10); err != nil {
		t.Fatalf("price: %v", err)
	}

	// The premise: in its OWN workspace the read works and returns the money. Without this an
	// empty result would satisfy the refusal assertion for the wrong reason.
	own, err := store.VersionCostSplit(ctx, pageID, []string{victimWS})
	if err != nil || !near(own.PageTotal, 0.5000) {
		t.Fatalf("premise: in-workspace read returned %+v, err=%v — want PageTotal 0.5000", own, err)
	}

	attackerWS := d.Workspace(t)
	got, err := store.VersionCostSplit(ctx, pageID, []string{attackerWS})
	if err == nil {
		t.Errorf("a caller in %s read %s's spend: %+v", attackerWS, victimWS, got)
	}
	if got.PageTotal != 0 {
		t.Errorf("refused read still returned PageTotal %.4f", got.PageTotal)
	}
}
