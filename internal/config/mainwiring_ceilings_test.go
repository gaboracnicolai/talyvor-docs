package config_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// A PINNED DEFAULT IS NOT AN ENFORCED ONE. shipped_defaults_test.go records the NUMBERS
// this service ships; this file records that cmd/docs/main.go actually HANDS each of them
// to something. They are two halves of one property and neither implies the other.
//
// ⚠⚠ MEASURED 2026-08-28 (tab-c5j7, W3.30), by mutation and not by reading:
// ~/talyvor-queue/w330-enforcement-census-c5j7.py replaces one `cfg.X` at its USE SITE in
// main.go with an unsafe literal, compiles, and runs the whole CI gauntlet against a real
// Postgres. Before this file existed the result was **9 of 9 NOT ENFORCED, 0 ENFORCED**:
//
//	AI 30/min -> 1e9 · search 240/min -> 1e9 · index 300/min -> 1e9 · index burst 10 -> 1e9
//	· index workers 4 -> 512 · re-embed staleness 5s -> 0 · /mcp body 4MB -> 1TB · /v1 body
//	4MB -> 1TB · import body 200MB -> 1TB
//
// Every one of those disconnects a shipped ceiling from the component that enforces it, and
// all 48 packages stayed green on every single one. The positive control — breaking the cap
// comparison inside internal/bodylimit, a package that IS tested — came back RED, so the
// nine greens were the product's and not the harness's.
//
// ⚠ WHY NOTHING SAW IT, and it is not an oversight in any one guard. `cmd/docs` has NO test
// files at all (`go list` reports TestGoFiles=[]), so all 750 lines of main.go are covered
// only by tests that read it AS SOURCE. The mainwiring family does exactly that — and
// internal/ratelimit/mainwiring_test.go states its own boundary in its header: it "CANNOT
// see a wrong argument". It pins that `WithRateLimit(` is CALLED on the constructor's
// chain. It cannot pin WHICH limiter, and no sibling pins which VALUE went into one. The
// argument is the half nobody was holding.
//
// ⚠ THIS FILE CHANGES NO VALUE AND MOVES NO CEILING. Every context below is the one already
// compiled into main.go today. Whether 30/min, 4MB or 200MB is the RIGHT number is a
// threshold on a spend path and is deliberately not taken here — the same posture
// shipped_defaults_test.go took about the same numbers.
//
// ⚠ AST, NOT GREP, and the direction matters: a grep census is a census of SPELLINGS, so a
// commented-out wiring line and a real one match the same pattern and the failure mode is a
// site that READS AS COVERED. Comments are not in the AST at all, so control C13 (a comment
// naming `cfg.MaxBodyBytes`) leaves this census untouched by construction rather than by a
// stripper that has to be correct. That is the lesson internal/ratelimit's quote-aware
// stripper paid for the hard way, avoided rather than re-implemented.
//
// ⚠ THE COMPARISON IS A MULTISET, NOT A SET, and control C7/C8 are why: `cfg.MaxBodyBytes`
// reaches bodylimit.Middleware TWICE (the /mcp group and the /v1 group). A set-valued table
// still contains "bodylimit.Middleware()" after one of the two is replaced by a literal, so
// it passes while half the service is uncapped. Counting is the whole guard there.
//
// ⚠ slog CONTEXTS ARE EXCLUDED ON PURPOSE. main.go logs several of these values at boot, and
// logging a number is not consuming it: a table that counted log sites would go red on any
// edit to a log line (and get relaxed away), while a ceiling replaced by a literal but still
// LOGGED from cfg would go green. Excluding them means the recorded set is exactly the set of
// real consumers. Control C14 pins that adding a log line does not move this census.
//
// Controls: ~/talyvor-queue/w330-mainwiring-controls-c5j7.py — 15 rows, 0 miss. C1-C9 are the
// nine mutations the census scored GREEN before this file existed; each is RED with it.

// mainPath is relative to this package's directory, matching the mainwiring family's
// convention (internal/ratelimit/mainwiring_test.go).
const ceilingMainPath = "../../cmd/docs/main.go"

// mainWiredConfig is the pinned wiring census: every `cfg.<Field>` main.go reads, against
// the CONSUMERS it is handed to. Sorted; duplicates are significant (see the multiset note).
//
// A field wired into main.go and absent here is a RED, not an omission — the population is
// recomputed from the file on every run, so a new configured value joining this class
// without being recorded fails rather than passing quietly. That is rule A of the mainwiring
// family, and the reason it exists: a guard that lists N sites reports a clean product
// forever after an N+1th is added.
var mainWiredConfig = map[string][]string{
	"AIRateBurst":           {"ratelimit.New()"},
	"AIRatePerMin":          {"ratelimit.New()"},
	"AllowedOrigins":        {".WithAllowedOrigins()"},
	"DatabaseURL":           {"db.New()"},
	"DefaultWorkspaceID":    {"trackintegration.NewSyncer()"},
	"GatewayAuthSecret":     {"gatewayauth.Middleware()", "gatewayauth.Middleware()"},
	"IndexRateBurst":        {"pageindex.Options{}.Burst"},
	"IndexRatePerMin":       {"pageindex.Options{}.RatePerSec"},
	"IndexStalenessSec":     {"pageindex.Options{}.Staleness"},
	"IndexWorkers":          {"pageindex.Options{}.Workers"},
	"LensAPIKey":            {"lenscreds.New()", "lensintegration.New()"},
	"LensURL":               {".WithLensURL()", "lenscreds.New()", "lensintegration.New()"},
	"ListenAddr":            {"http.Server{}.Addr", "listenHostname()"},
	"MaxBodyBytes":          {"bodylimit.Middleware()", "bodylimit.Middleware()"},
	"MaxImportBodyBytes":    {"bodylimit.Middleware()"},
	"SearchRateBurst":       {"ratelimit.New()"},
	"SearchRatePerMin":      {"ratelimit.New()"},
	"SlogLevel":             {"cfg.SlogLevel()"},
	"TrackAPIKey":           {"trackintegration.New()"},
	"TrackMemberSyncSecret": {".WithMemberSyncSecret()"},
	"TrackURL":              {"trackintegration.New()"},
}

// ceilingFloor is the vacuity floor. A scanner that finds nothing agrees with any table, so
// "found no wiring" must be LOUD rather than a pass. Set below today's count so ordinary
// removals surface through the diff below rather than here, but far enough above zero that a
// parser pointed at the wrong file, or one whose walk silently stops matching, cannot be
// mistaken for a clean tree.
const ceilingFloor = 15

// qualifiedName renders a callee or composite-literal type as a stable label.
// `pkg.Func` for a package-level call, `.Method` for a call on a chain (the receiver is an
// expression, not a name, so the method is the stable half — cf. the constructor-vs-variable
// finding in internal/ratelimit/mainwiring_test.go).
func qualifiedName(e ast.Expr) string {
	s, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if id, ok := s.X.(*ast.Ident); ok {
		return id.Name + "." + s.Sel.Name
	}
	return "." + s.Sel.Name
}

// censusMainWiring returns field -> sorted consumer labels, excluding slog sites.
func censusMainWiring(t *testing.T, path string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	parent := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) > 0 {
			parent[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})

	got := map[string][]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name != "cfg" {
			return true
		}
		label, ok := consumerOf(parent, n)
		if !ok {
			return true // a slog site: logging a value is not consuming it
		}
		got[sel.Sel.Name] = append(got[sel.Sel.Name], label)
		return true
	})
	for k := range got {
		sort.Strings(got[k])
	}
	return got
}

// isConversion lists the type conversions main.go wraps a configured value in on the way to
// its consumer. They are STEPPED THROUGH rather than reported, so `time.Duration(cfg.X)` is
// attributed to the field it initialises and not to the conversion. Named explicitly: the
// first draft skipped every unqualified call instead, which silently swallowed real
// consumers (control C11).
var isConversion = map[string]bool{
	"time.Duration": true,
	"int":           true,
	"int32":         true,
	"int64":         true,
	"float64":       true,
	"string":        true,
}

// consumerOf walks up from a `cfg.X` selector to the nearest thing that CONSUMES it,
// stepping through `time.Duration(...)` conversions and parenthesised/binary expressions
// (main.go writes `cfg.IndexRatePerMin / 60` and
// `time.Duration(cfg.IndexStalenessSec) * time.Second`). Reports ok=false for a slog site.
func consumerOf(parent map[ast.Node]ast.Node, n ast.Node) (string, bool) {
	cur := n
	for i := 0; i < 12; i++ {
		p, ok := parent[cur]
		if !ok {
			break
		}
		switch x := p.(type) {
		case *ast.CallExpr:
			name := qualifiedName(x.Fun)
			if name == "" {
				// An UNQUALIFIED call, `newSink(cfg.X)`. This is a CONSUMER named by its
				// identifier, not a conversion.
				//
				// ⚠ IT WAS CLASSIFIED AS A CONVERSION IN THE FIRST DRAFT AND CONTROL C11
				// CAUGHT IT. Walking past an unnamed call means a value handed to a local
				// function is credited to whatever encloses that call — and when the
				// enclosing call is a slog site, the value is DROPPED FROM THE CENSUS
				// ENTIRELY. `newSink(cfg.LogLevel)` inside a slog.Info(...) was invisible:
				// rule A stayed green with a configured value wired in and unrecorded,
				// which is the exact failure rule A exists to prevent.
				if id, ok := x.Fun.(*ast.Ident); ok {
					name = id.Name
				} else {
					cur = p // a func literal or other non-name callee — keep walking
					continue
				}
			}
			if isConversion[name] {
				cur = p // a conversion wrapping the value, not a consumer of it
				continue
			}
			if strings.HasPrefix(name, "slog.") {
				return "", false
			}
			return name + "()", true
		case *ast.KeyValueExpr:
			if cl, ok := parent[p].(*ast.CompositeLit); ok {
				if key, ok := x.Key.(*ast.Ident); ok {
					return qualifiedName(cl.Type) + "{}." + key.Name, true
				}
			}
		}
		cur = p
	}
	return "<unattributed>", true
}

// TestMain_PassesEveryConfiguredValueToItsConsumer is the guard the 9-of-9 census above
// asked for: it fails when a configured value stops reaching the component that enforces it.
func TestMain_PassesEveryConfiguredValueToItsConsumer(t *testing.T) {
	got := censusMainWiring(t, ceilingMainPath)

	// ── vacuity. A census that found nothing agrees with every table.
	total := 0
	for _, v := range got {
		total += len(v)
	}
	if total < ceilingFloor {
		t.Fatalf("the wiring census found only %d consumer sites in %s (floor %d). A scanner "+
			"that returns nothing agrees with ANY table, so this is a broken census rather "+
			"than a clean main.go — check the path and the AST walk before touching the table.",
			total, ceilingMainPath, ceilingFloor)
	}

	// ── rule A: the population is recomputed, so a NEW configured value must be recorded.
	for field, consumers := range got {
		if _, recorded := mainWiredConfig[field]; !recorded {
			t.Errorf("cfg.%s is wired in %s (to %s) but is NOT in mainWiredConfig.\n"+
				"A configured value that reaches a consumer without being recorded is exactly "+
				"what this table exists to prevent: add it, so that disconnecting it later is a red.",
				field, ceilingMainPath, strings.Join(consumers, ", "))
		}
	}

	// ── rule B: each recorded value must still reach the SAME consumers, counted.
	for field, want := range mainWiredConfig {
		have, present := got[field]
		if !present {
			t.Errorf("cfg.%s NO LONGER REACHES ANY CONSUMER in %s — expected %s.\n"+
				"The value is still pinned by shipped_defaults_test.go, so config.Load() will "+
				"keep returning it and every other test will keep passing while the component "+
				"that enforces it runs on something else. That is the 9-of-9 defect this file "+
				"was written for.", field, ceilingMainPath, strings.Join(want, ", "))
			continue
		}
		if !sameMultiset(have, want) {
			t.Errorf("cfg.%s wiring changed in %s:\n  recorded: %s\n  found:    %s\n"+
				"Counts are significant — a value reaching two call sites and now reaching one "+
				"leaves the other running on a literal.",
				field, ceilingMainPath, strings.Join(want, ", "), strings.Join(have, ", "))
		}
	}
}

func sameMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// TestMain_WiringCensusIsAttributable fails if the walk cannot name what consumes a value.
// An "<unattributed>" row is not a defect in main.go — it is this file failing to describe
// it, and a census that quietly buckets sites it does not understand will bucket a REAL
// regression there too.
func TestMain_WiringCensusIsAttributable(t *testing.T) {
	for field, consumers := range censusMainWiring(t, ceilingMainPath) {
		for _, c := range consumers {
			if c == "<unattributed>" {
				t.Errorf("cfg.%s reaches a site consumerOf() cannot name. Teach the walk that "+
					"shape rather than recording %q, or the same bucket will swallow a real "+
					"disconnection.", field, c)
			}
		}
	}
}
