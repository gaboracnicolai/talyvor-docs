package mcp_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// THE MCP SURFACE REPORTED THE TRACK HALF OF A PAGE'S COST AND IT WAS THE ONLY COST FIELD ON IT.
//
// A page carries two independent cost columns and a derived sum (migration 0018, model.Page):
//
//	ai_cost_usd        the cost of the Track ISSUES linked to this page. Recomputed and overwritten
//	                   by a sweep.
//	own_ai_cost_usd    the cost of AI operations performed ON this document. Accumulated.
//	total_ai_cost_usd  their sum, derived on read by page.withDerivedTotal.
//
// The page JSON carries all three. `8b3e1be` (#54) fixed the REST search projection, which carried
// `ai_cost_usd` alone. THE MCP get_page TOOL IS THE SECOND COPY OF THAT SEAM and #54 did not
// touch it: pageOut declared `AICostUSD float64` filled from `p.AICostUSD`.
//
// ⚠ MEASURED ON THE SHIPPED TOOL over real Postgres through the whole JSON-RPC chain, at
// `8b3e1be`, before this guard existed:
//
//	A: own 12.34, track 0     ->  "ai_cost_usd": 0     (own/total absent)
//	B: own 0,     track 1.50  ->  "ai_cost_usd": 1.5   (own/total absent)
//	C: own 0,     track 0     ->  "ai_cost_usd": 0     (own/total absent)
//
// A AND C ARE BYTE-IDENTICAL. The document whose entire spend was its own AI work is
// indistinguishable, to the AI agent reading it, from one that cost nothing.
//
// ⚠ AND IT IS STRICTLY WORSE HERE THAN IT WAS IN SEARCH, WHICH IS WHY THIS IS ITS OWN MERGE AND
// NOT A COPY OF #54's. The search row put `omitempty` on a bare float, so page A emitted NO cost
// field — a reader got silence. pageOut has no omitempty, so page A emits a concrete numeral
// `0`: not a gap, a positive assertion that this document cost nothing. Silence can be noticed;
// a fabricated zero cannot.
//
// ⚠ AND THE MCP SURFACE HAS NO OTHER ROUTE TO THE MISSING HALF. Measured by enumerating the
// COMPLETE emitted vocabulary of all 10 tools (every `json:"..."` tag in the non-test sources of
// this package): exactly one of them is a cost, and it is this one. `get_page_analytics` carries
// views only; `search_docs`'s searchHit carries no cost at all. own_ai_cost_usd was unreachable
// through MCP entirely, by any tool, in any call.
//
// WHY BARE FLOATS HERE AND POINTERS IN SEARCH — the shapes differ because the PREDICATE differs,
// not by preference. #54 needed *float64 because a SEMANTIC-ONLY search row is built from a
// vector hit with no `pages` row read, so 0.0 there would be a fabricated zero: three states
// (measured-zero / not-reported / a number) and a bare float expresses two. pageOut is built at
// exactly ONE site, from a nil-checked *model.Page returned by GetByID or GetBySlug — both of
// which go through page.scan, which fills all three. There is no not-reported state to express,
// so a bare float is the honest shape and a pointer would be inherited provenance.
//
// DECODED INTO A MAP, NOT INTO pageOut. A typed decode cannot tell "reported as zero" from "not
// reported at all" — which is the whole of the defect for page A — and a test that decoded into
// the struct would have passed through the defect's entire life.
func TestMCPGetPageReportsBothCostHalves_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	bob := d.Member(t, ws, "bob@corp.com")

	// D carries BOTH halves non-zero and is what holds total_ai_cost_usd to being a SUM rather
	// than a copy of either addend: with A/B/C alone, `total = own` and `total = track` are each
	// correct on two of the three pages.
	cases := []struct {
		name              string
		own, track, total float64
	}{
		{"A own-funded", 12.34, 0, 12.34},
		{"B track-funded", 0, 1.5, 1.5},
		{"C genuinely free", 0, 0, 0},
		{"D both halves", 12.34, 1.5, 13.84},
	}
	chain := newMCPChain(t, d)

	for _, c := range cases {
		id := d.Page(t, ws, bob, c.name)
		if _, err := d.Pool.Exec(ctx,
			`UPDATE pages SET own_ai_cost_usd = $1, ai_cost_usd = $2 WHERE id = $3`,
			c.own, c.track, id); err != nil {
			t.Fatalf("%s: seed: %v", c.name, err)
		}

		got := mcpPageJSON(t, chain, id)
		for _, f := range []struct {
			key  string
			want float64
		}{
			{"ai_cost_usd", c.track},
			{"own_ai_cost_usd", c.own},
			{"total_ai_cost_usd", c.total},
		} {
			raw, present := got[f.key]
			if !present {
				t.Errorf("%s: get_page emitted NO %q. The page's cost is %v (track) + %v (own); an "+
					"agent reading this document through MCP cannot see it. Payload: %v",
					c.name, f.key, c.track, c.own, got)
				continue
			}
			v, ok := raw.(float64)
			if !ok {
				t.Errorf("%s: %q = %#v, want a number", c.name, f.key, raw)
				continue
			}
			// Epsilon rather than ==: total is summed in float64 before it is serialised, so this
			// asserts WHICH COLUMN reached the wire, not decimal formatting.
			if math.Abs(v-f.want) > 1e-9 {
				t.Errorf("%s: %q = %v, want %v — get_page is reporting the wrong half of this "+
					"page's cost. Payload: %v", c.name, f.key, v, f.want, got)
			}
		}
	}
}

// mcpPageJSON drives the real JSON-RPC chain and returns get_page's tool payload as a MAP, so an
// absent key is distinguishable from a zero one.
func mcpPageJSON(t *testing.T, chain http.Handler, pageID string) map[string]any {
	t.Helper()
	rr := callTool(chain, "bob@corp.com", true, "get_page", map[string]any{"page_id": pageID})
	if rr.Code != http.StatusOK {
		t.Fatalf("get_page %s: HTTP %d — %s", pageID, rr.Code, rr.Body.String())
	}
	var env struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("get_page %s: decode envelope: %v — %s", pageID, err, rr.Body.String())
	}
	if len(env.Error) > 0 {
		t.Fatalf("get_page %s: rpc error %s", pageID, env.Error)
	}
	if len(env.Result.Content) == 0 {
		t.Fatalf("get_page %s: no tool content — %s", pageID, rr.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(env.Result.Content[0].Text), &m); err != nil {
		t.Fatalf("get_page %s: decode tool payload: %v — %s", pageID, err, env.Result.Content[0].Text)
	}
	return m
}
