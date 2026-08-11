package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// WHAT "COST-AWARE SEARCH" SAYS A DOCUMENT COST.
//
// A page has two costs and migration 0018 / #52 exists because conflating them was the defect:
//
//	ai_cost_usd        the cost of the Track ISSUES linked to this page. Overwritten by a sweep.
//	own_ai_cost_usd    the cost of AI operations performed ON this document. Accumulated.
//	total_ai_cost_usd  their sum, derived on read.
//
// The page JSON carries all three. Search carried `ai_cost_usd` alone — the Track half, i.e.
// exactly the half 0018 was written because it was missing — and tagged it `omitempty`, which on a
// bare float means the field VANISHES when that half is 0.
//
// MEASURED at `ad20b1c` over two documents matching the same query:
//
//	A: own_ai_cost_usd 12.34, ai_cost_usd 0  ->  search emitted NO cost field at all
//	B: own_ai_cost_usd 0, ai_cost_usd 1.50   ->  search emitted "ai_cost_usd": 1.5
//
// So the document that actually cost money in Docs read as free, the one funded from Track read as
// the expensive one, and a reader comparing them got the answer exactly backwards. `ai_cost_usd`
// appeared ZERO times across all four test files in this package, so nothing said otherwise.
//
// ⚠ THIS TEST DECODES INTO A MAP, NOT INTO Result. A typed decode cannot tell "reported as zero"
// from "not reported" — both arrive as 0.0 — and that difference is the entire defect for page A.
func TestSearch_RealPG_ReportsBothHalvesOfAPagesCost(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")

	own := d.Page(t, ws, author, "Runbook Alpha")  // spend on the DOCUMENT
	track := d.Page(t, ws, author, "Runbook Beta") // spend on its linked ISSUES
	free := d.Page(t, ws, author, "Runbook Gamma") // genuinely zero, on both columns
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET content_text = 'the deployment runbook', own_ai_cost_usd = 12.34, ai_cost_usd = 0 WHERE id = $1`, own); err != nil {
		t.Fatalf("seed own-spend page: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET content_text = 'the deployment runbook', own_ai_cost_usd = 0, ai_cost_usd = 1.5 WHERE id = $1`, track); err != nil {
		t.Fatalf("seed track-spend page: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET content_text = 'the deployment runbook', own_ai_cost_usd = 0, ai_cost_usd = 0 WHERE id = $1`, free); err != nil {
		t.Fatalf("seed zero-cost page: %v", err)
	}

	body := searchAsMaps(t, d, ws, author, "runbook")
	if len(body) != 3 {
		t.Fatalf("want 3 results, got %d", len(body))
	}
	byID := map[string]map[string]any{}
	for _, r := range body {
		byID[r["page_id"].(string)] = r
	}

	// The document whose own AI spend is the whole of its cost.
	assertCost(t, byID[own], "the page whose cost is its OWN AI spend", 0, 12.34, 12.34)
	// The document whose cost comes from its linked Track issues.
	assertCost(t, byID[track], "the page whose cost is its linked ISSUES", 1.5, 0, 1.5)
	// ⚠ A REPORTED ZERO IS NOT A MISSING FIELD. This is what stops the fix from being "drop
	// omitempty and hope": a page that genuinely cost nothing must SAY zero, so a reader can tell
	// "nothing was spent here" from "this surface does not report it".
	assertCost(t, byID[free], "the page that genuinely cost nothing", 0, 0, 0)
}

func assertCost(t *testing.T, row map[string]any, what string, wantAI, wantOwn, wantTotal float64) {
	t.Helper()
	if row == nil {
		t.Fatalf("%s: missing from the results entirely", what)
	}
	for field, want := range map[string]float64{
		"ai_cost_usd":       wantAI,
		"own_ai_cost_usd":   wantOwn,
		"total_ai_cost_usd": wantTotal,
	} {
		raw, present := row[field]
		if !present {
			t.Errorf("%s: %q is ABSENT from the search result. The page JSON reports it; this "+
				"surface silently does not, so a document with real spend reads as free.", what, field)
			continue
		}
		got, ok := raw.(float64)
		if !ok {
			t.Errorf("%s: %q = %v (%T), want a number", what, field, raw, raw)
			continue
		}
		if got < want-1e-9 || got > want+1e-9 {
			t.Errorf("%s: %q = %v, want %v", what, field, got, want)
		}
	}
}

// ⚠ THE OTHER DIRECTION, AND THE REASON THE FIELDS ARE POINTERS RATHER THAN BARE FLOATS.
//
// A semantic-only row (a page whose embedding matched but whose full-text index did not fire)
// carries no cost, so emitting 0.0 there would be a fabricated zero: it would claim a document cost
// nothing on the strength of never having reported. The field must be ABSENT for those rows and
// PRESENT-AND-ZERO for a page whose cost WAS read, which a bare float cannot express and a
// *float64 can.
//
// ⚠ THIS COMMENT USED TO SAY "no pages row is read for it", AND THAT IS FALSE — see the note on
// Result in handler.go. SemanticSearch.Search JOINs `pages` and filters three of its columns; the
// cost columns are on that same already-joined row and are simply not selected. The costs are
// absent by CHOICE, not by cost, and that choice is a money-surface question nobody has answered.
func TestSearch_SemanticOnlyRow_ReportsNoCostRatherThanAFabricatedZero(t *testing.T) {
	r := Result{PageID: "pg-1", Source: "semantic", Similarity: 0.9}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"ai_cost_usd", "own_ai_cost_usd", "total_ai_cost_usd"} {
		if v, present := m[field]; present {
			t.Errorf("a semantic-only row emitted %q = %v. Nothing read that page's cost, so a "+
				"number here is invented rather than measured.", field, v)
		}
	}
}

func searchAsMaps(t *testing.T, d *testutil.DB, ws, author, q string) []map[string]any {
	t.Helper()
	s := page.NewStore(d.Pool)
	sem := newSemanticSearch(lensintegration.New("", ""), nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authz.WithMemberships(req.Context(), "alice@example.com",
				[]authz.Membership{{WorkspaceID: ws, MemberID: author}})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/v1", func(r chi.Router) { NewHandler(s, sem).Mount(r) })

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/search?q="+q, nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /search = HTTP %d %s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rr.Body.String())
	}
	return envelope.Results
}
