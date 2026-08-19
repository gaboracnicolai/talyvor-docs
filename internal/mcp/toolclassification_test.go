package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"testing"
)

// THE THREE HAND-MAINTAINED TOOL SETS, AND THE POPULATION THEY ARE SUPPOSED TO COVER.
//
// llmTools, writeTools and gatedReadTools each carry a comment asking the next author to keep the
// set honest — "a new tool that calls the AI engine belongs here, or it is an unmetered hole",
// "a new mutating tool that is not listed here bypasses the tier gate, the same class of hole this
// closes for create/update/verify_page". Both sentences describe a hazard that arrives with a tool
// nobody has written yet, and until this file NOTHING IN THE REPOSITORY COULD SEE IT: every
// existing test names an existing tool, so the sets are covered exactly where they are already
// right.
//
// ⚠⚠ MEASURED AT 14a6abc, NOT ARGUED. A new MUTATING tool was added to callTool's dispatch —
// `case "archive_page": return s.toolUpdatePage(ctx, args)`, reachable through the chokepoint via
// toolWorkspace's page arm — and listed in NO set. `gofmt -l` clean, `go vet ./...` exit 0, and
// `go test -timeout 600s -race -count=1 ./...` against a real Postgres, 36 packages, **EXIT 0**.
// A view-tier member could mutate a page through it: writeTools[name] is false, so the tier gate
// never runs. That is the hole writeTools' own comment says it closes, and the whole gauntlet was
// green with it armed.
//
// ⚠ THE POPULATION IS RECOMPUTED FROM THE DISPATCH SWITCH ON EVERY RUN, WHICH IS THE WHOLE POINT.
// A guard that iterated the three sets would be satisfied by construction — every member of a set
// is in a set — and would have watched `archive_page` go in. It parses server.go because the
// dispatch switch is the only place that says which tool names the server actually SERVES.
//
// ⚠ TWO CLASSIFICATIONS, NOT ONE, AND THEY ARE INDEPENDENT. llmTools answers "does this spend?"
// and writeTools/gatedReadTools answer "what may the caller do?". A tool can need an answer to one
// and not the other, so conflating them would let a new metered tool skip the tier gate merely by
// being metered. Each rule has its own exempt list and its own control (see the harness).
//
// ⚠ AND THE BLINDNESS CONTROL WAS WRONG BEFORE THE GUARD WAS. T6 blinds the population to the
// classification sets and expects everything to go green with T1's defect armed. Its first version
// ALSO added the fake tool to the exempt lists — and since the blinded population is BUILT from
// those lists, the addition put the fake tool straight back into the population and
// TOOL-UNADVERTISED fired. The control was measuring its own fixture. Blinding alone is the
// mutation, and the corrected T6 is GREEN: eight controls, all as predicted, in
// scripts/w31-toolclass-controls-8f5c.py.
//
// ⚠ WHAT THIS GUARD IS AND IS NOT, in the words internal/ratelimit/mainwiring_test.go established.
// It reads the dispatch switch, so it catches a tool being SERVED without a decision having been
// recorded about it. It CANNOT see a tool reached some other way (a helper that dispatches on a
// name of its own), it CANNOT judge whether a chosen classification is the RIGHT one — putting a
// mutating tool in readExemptTools with a false reason passes — and it is not a substitute for
// sec_tier_test.go, which proves the gate actually denies. It closes the gap between "someone
// decided" and "nobody noticed", which is the gap the measurement above found.

// accessExemptTools are the dispatched tools that are in NEITHER writeTools NOR gatedReadTools,
// each with the reason it is gated somewhere else instead. A new tool must be named here — with a
// reason — or in one of the two sets, and either way the decision is a visible line in a diff.
//
// ⚠ THIS LIST IS ALSO A CORRECTION. gatedReadTools' own comment announced "THE THREE READ TOOLS
// THAT ARE NOT HERE" and named search_docs, get_space_tree and get_page. Measured against the
// dispatch switch there are FOUR — get_stale_pages is gated in internal/freshness, one layer
// further out than the other three, and the comment never mentioned it. The count was written when
// it was true and no test could see it stop being true; now the list is machine-checked and the
// comment has been corrected to match.
var accessExemptTools = map[string]string{
	"search_docs":     "returns a MIXED set the caller did not enumerate; filters per row inside the tool over an over-fetched window",
	"get_space_tree":  "same mixed-set shape, filtered per SPACE inside the tool",
	"get_page":        "its object does not exist until the lookup has run (the slug arm carries only space_id+slug), so it gates inside the tool",
	"get_stale_pages": "filtered per row one layer further out, by freshness.GetStaleReport's AuthorizePageRead pass",
	"ask_docs":        "filters its GROUNDING CORPUS per page inside the tool, before the rows reach the model",
}

// spendExemptTools are the dispatched tools that are NOT in llmTools, each with the reason it does
// not reach Lens. The page-save embed path is deliberately not counted here: create_page and
// update_page do cause an embed, but it is enqueued on internal/pageindex's throttle rather than
// billed to this call, and that throttle is the ceiling for it.
var spendExemptTools = map[string]string{
	"search_docs":        "SearchWithRank is lexical SQL; the semantic (embedding) half is search.Handler's, not this tool's",
	"get_page":           "one row read",
	"get_space_tree":     "space + page reads",
	"get_stale_pages":    "a TTL predicate in SQL",
	"list_pages":         "one listing query",
	"get_page_analytics": "one stats query",
	"create_page":        "a write; the embed it triggers is enqueued on internal/pageindex's throttle, which is that path's ceiling",
	"update_page":        "same as create_page",
	"verify_page":        "a freshness attestation, no model call",
}

// dispatchedTools recomputes the population: the tool names callTool's switch actually serves.
func dispatchedTools(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("[TOOLPOP-UNREADABLE] could not parse server.go: %v", err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "callTool" || fn.Recv == nil {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			sw, ok := m.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "name" {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					lit, ok := expr.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if s, err := strconv.Unquote(lit.Value); err == nil {
						names = append(names, s)
					}
				}
			}
			return false
		})
		return false
	})
	sort.Strings(names)
	// A FLOOR, because an empty population is the failure mode this whole file would be silent
	// about: a renamed function or a restructured switch would leave every rule below iterating
	// nothing and reporting a healthy server.
	if len(names) < 8 {
		t.Fatalf("[TOOLPOP-FLOOR] the dispatch switch yielded only %d tool names (%v) — the "+
			"population is being read wrong, and every rule in this file would pass over it", len(names), names)
	}
	return names
}

func TestEveryDispatchedTool_HasAnAccessDecision(t *testing.T) {
	for _, name := range dispatchedTools(t) {
		_, write := writeTools[name]
		_, read := gatedReadTools[name]
		reason, exempt := accessExemptTools[name]
		switch {
		case write && read:
			t.Errorf("[TOOL-ACCESS-BOTH] %q is in BOTH writeTools and gatedReadTools", name)
		case (write || read) && exempt:
			t.Errorf("[TOOL-ACCESS-BOTH] %q is gated at dispatch AND listed as exempt: %q", name, reason)
		case !write && !read && !exempt:
			t.Errorf("[TOOL-ACCESS-UNCLASSIFIED] %q is SERVED by callTool and appears in neither "+
				"writeTools nor gatedReadTools nor accessExemptTools. If it mutates, the tier gate "+
				"never runs for it and a view-tier member can call it; if it reads, say where it is "+
				"gated instead. Measured: a tool in this state passes the entire gauntlet.", name)
		case exempt && reason == "":
			t.Errorf("[TOOL-ACCESS-NO-REASON] %q is exempt with an empty reason", name)
		}
	}
}

func TestEveryDispatchedTool_HasASpendDecision(t *testing.T) {
	for _, name := range dispatchedTools(t) {
		metered := llmTools[name]
		reason, exempt := spendExemptTools[name]
		switch {
		case metered && exempt:
			t.Errorf("[TOOL-SPEND-BOTH] %q is in llmTools AND listed as not reaching Lens: %q", name, reason)
		case !metered && !exempt:
			t.Errorf("[TOOL-SPEND-UNCLASSIFIED] %q is SERVED by callTool and is in neither llmTools "+
				"nor spendExemptTools. If it reaches Lens it is an unmetered hole on the agent door — "+
				"the ceiling is keyed on llmTools alone.", name)
		case exempt && reason == "":
			t.Errorf("[TOOL-SPEND-NO-REASON] %q is spend-exempt with an empty reason", name)
		}
	}
}

// The classification sets say what a tool MAY do; tools/list says what a caller is TOLD exists.
// A tool that dispatches without being advertised is a tool no reviewer reads the schema of, and
// the rules above cannot see it because they are keyed on the dispatch switch too.
func TestAdvertisedTools_AreExactlyTheDispatchedOnes(t *testing.T) {
	srv := newServer(deps{version: "test"})
	advertised := map[string]bool{}
	for _, spec := range srv.toolsList() {
		advertised[spec.Name] = true
	}
	dispatched := map[string]bool{}
	for _, name := range dispatchedTools(t) {
		dispatched[name] = true
		if !advertised[name] {
			t.Errorf("[TOOL-UNADVERTISED] %q is served by callTool and absent from tools/list", name)
		}
	}
	for name := range advertised {
		if !dispatched[name] {
			t.Errorf("[TOOL-UNDISPATCHED] %q is advertised by tools/list and served by nothing — a "+
				"caller following the schema gets method-not-found", name)
		}
	}
}
