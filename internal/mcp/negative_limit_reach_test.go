package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// THE `search_docs` TOOL PASSES A CALLER-SUPPLIED `limit` THROUGH A MULTIPLY WITH NO LOWER BOUND,
// AND `page.Store.SearchWithRank`'s OWN `limit <= 0` CORRECTION IS THE ONLY THING BETWEEN AN MCP
// ARGUMENT AND A `LIMIT -4` REACHING POSTGRES.
//
// `server.go`'s toolSearchDocs does `limit := intArg(args, "limit", 5)` and then, when the access
// gate is wired, `fetchLimit = limit * searchFetchFactor` with an UPPER clamp only. `intArg` is
// `if v, ok := args[name].(float64); ok { return int(v) }` — no range check, and converting an
// out-of-range float64 to int is implementation-defined, so a JSON `1e30` also arrives negative.
//
// ⚠ MEASURED THROUGH THIS TOOL, with the store correction temporarily neutered: `limit` of -1, -5,
// 9.3e18 and 1e30 each returned a JSON-RPC error carrying Postgres' own text —
// `{"code":-32603,"message":"ERROR: LIMIT must not be negative (SQLSTATE 2201W)"}`. With the
// correction in place all four return results. NO OVERFLOW IS REQUIRED: a plain `-1` is enough.
//
// ⚠ SO THE CORRECTION IS NOT AN UNREACHABLE BACKSTOP (W4.44's true-row case) — it is reached from
// a shipping tool by an ordinary argument, and it was measured UNTESTED (W3.58, arm B1: neuter it
// and the whole suite stays green).
//
// ⚠ `limit=0` IS THE CASE A STATUS-ONLY ASSERTION WOULD MISS. `LIMIT 0` is legal SQL that returns
// NOTHING, so without the correction that request answers 200 with an empty list rather than
// erroring — the answer changes and nothing fails. The row count is asserted for that reason.
//
// ⚠ THE MUTATION USED TO VERIFY THIS MUST BE LINE-PRESERVING. Deleting the correction's three
// lines reds `internal/sqlguard` on its own account (its `unreconstructable` exemption list is
// keyed by file:line), which reads as a catch and is not one.
func TestMCPSearchDocs_NegativeLimit_IsCorrectedBeforeSQL_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	for _, title := range []string{"Runbook alpha", "Runbook beta", "Runbook gamma"} {
		id := d.Page(t, ws, alice, title)
		if _, err := d.Pool.Exec(context.Background(),
			`UPDATE pages SET content_text = $2 WHERE id = $1`, id, title+" body text"); err != nil {
			t.Fatalf("seed content_text: %v", err)
		}
	}
	chain := newMCPChain(t, d)

	// hits returns the number of rows the tool reported, and fails on a JSON-RPC error rather than
	// treating it as zero rows — "the tool errored" and "the tool found nothing" are the two
	// outcomes this test has to tell apart, and HTTP is 200 for both.
	hits := func(t *testing.T, limit any) int {
		t.Helper()
		rr := callTool(chain, "alice@corp.com", true, "search_docs",
			map[string]any{"query": "Runbook", "workspace_id": ws, "limit": limit})
		var resp struct {
			Error  *struct{ Message string } `json:"error"`
			Result *struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("limit=%v: decode: %v (%s)", limit, err, rr.Body.String())
		}
		if resp.Error != nil {
			t.Fatalf("limit=%v: tool errored: %s\n"+
				"With page.Store.SearchWithRank's `limit <= 0 -> 10` correction gone this is "+
				"Postgres' own \"LIMIT must not be negative\", relayed to an MCP client verbatim.",
				limit, resp.Error.Message)
		}
		if resp.Result == nil || len(resp.Result.Content) == 0 {
			t.Fatalf("limit=%v: no content in result: %s", limit, rr.Body.String())
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &rows); err != nil {
			t.Fatalf("limit=%v: decode payload: %v (%s)", limit, err, resp.Result.Content[0].Text)
		}
		return len(rows)
	}

	// PREMISE + NON-VACUITY. An ordinary limit must find the three seeded pages, or "no error"
	// below would be indistinguishable from "this tool searches nothing".
	if n := hits(t, 5); n != 3 {
		t.Fatalf("PREMISE FAILED: limit=5 returned %d rows, want 3 — the instrument cannot see "+
			"the seeded pages, so nothing below discriminates anything", n)
	}
	// COUNTERWEIGHT: the tool's own truncation still applies for a POSITIVE limit. Without this a
	// tool that ignored `limit` entirely would satisfy every case below.
	if n := hits(t, 2); n != 2 {
		t.Fatalf("limit=2 returned %d rows, want 2 — the positive-limit truncation must still bite", n)
	}

	for _, tc := range []struct {
		name  string
		limit any
	}{
		// `limit > 0 && len(hits) >= limit` guards the tool's truncation, so a non-positive limit
		// truncates to nothing and the row count is the CORRECTED fetch (10, capped by the corpus).
		{"negative", -1},
		{"more negative", -5},
		{"float64 past MaxInt64", 9.3e18},
		{"float64 far past MaxInt64", 1e30},
		{"zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if n := hits(t, tc.limit); n != 3 {
				t.Fatalf("limit=%v returned %d rows, want 3 (the whole corpus, since a "+
					"non-positive limit disables the tool's truncation) — `LIMIT 0` is legal SQL "+
					"returning nothing, so the COUNT is what separates corrected from "+
					"silently-emptied", tc.limit, n)
			}
		})
	}
}
