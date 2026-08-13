package mcp

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// get_stale_pages's description is a CONTRACT AN AGENT ACTS ON, and at 9e0ac54 it promised a
// criterion the tool's population cannot apply: "past their stale_after_days threshold OR with
// linked Track issues completed since the last edit".
//
// The whole population of that tool is freshness.GetStaleReport → staleReportAll →
// page.Store.GetStalePages, one SQL predicate over stale_after_days. The linked-issue signal is
// computed only for pages that predicate already returned, so it ANNOTATES a row and can never
// add one. Measured on real Postgres, with the link store and the Track reader genuinely wired,
// by internal/freshness/stalereport_population_realpg_test.go — read its [CONTROL] tag for why
// the absence it measures is a boundary rather than a dead fixture.
//
// ⚠ THIS IS THE RED-FIRST HALF OF THAT MERGE. It fails on the unmodified tree; the behavioural
// guard beside it does not, and says so in its own header.
//
// ⚠ IT DRIVES THE REAL tools/list RPC RATHER THAN READING THE Go LITERAL, because what an agent
// receives is the serialised field, and a description that never reached the wire would satisfy
// a struct assertion.
//
// ⚠ AND IT ASSERTS BOTH DIRECTIONS. A one-sided "must not mention linked issues" is quietened
// for free by deleting the description — which leaves an agent with LESS than the false version
// gave it. So the criterion the route does apply must still be named, and the route that does
// carry the linked-issue signal (get_page) must still be pointed at. Wording beyond those three
// facts is deliberately unconstrained.
func TestGetStalePagesDescription_MatchesItsPopulation(t *testing.T) {
	srv := newTestServer(t, &fakePages{}, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, rpc("tools/list", nil, 1))
	resp := decodeResp(t, rr)
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)

	var desc string
	var found bool
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["name"] == "get_stale_pages" {
			found = true
			desc, _ = tool["description"].(string)
		}
	}
	if !found {
		t.Fatal("[DESC-PRESENT] get_stale_pages is not in tools/list — this guard would pass " +
			"vacuously on a tool that no longer ships")
	}
	if desc == "" {
		t.Fatal("[DESC-PRESENT] get_stale_pages ships an empty description; an agent choosing " +
			"between ten tools reads this field, and empty is not an improvement on false")
	}

	low := strings.ToLower(desc)
	if !strings.Contains(low, "stale_after_days") {
		t.Errorf("[DESC-NAMES-REAL-CRITERION] the description no longer names the ONE criterion "+
			"the population applies: %q", desc)
	}
	if strings.Contains(low, "or with linked track issues completed") {
		t.Errorf("[DESC-NO-FALSE-OR] the description promises that completed linked Track issues "+
			"put a page in this list; TestStaleReport_PopulationIsTTLOnly_RealPG measures that "+
			"they cannot. If the population really was widened, that test is red too and BOTH "+
			"move together: %q", desc)
	}
	if !strings.Contains(low, "get_page") {
		t.Errorf("[DESC-POINTS-AT-SIGNAL] the description states the boundary without saying "+
			"where the linked-issue signal DOES live (get_page), which leaves an agent with a "+
			"dead end instead of a next call: %q", desc)
	}
}
