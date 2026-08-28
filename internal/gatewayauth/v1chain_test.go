package gatewayauth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// THE /v1 SECURITY CHAIN — THE ONE MUTATION NOTHING IN THIS REPOSITORY COULD SEE.
//
// ⚠⚠ MEASURED 2026-08-28 (tab-c5j7, W3.34) by deleting each security middleware from
// cmd/docs/main.go and running the whole CI gauntlet against a real Postgres
// (~/talyvor-queue/w334-securitywiring-census-c5j7.py). Population derived from the AST, four
// sites, and the results do NOT agree with how covered they look:
//
//	gatewayauth.Middleware on /mcp   RED   by mcpchain_test.go — BY SUBJECT
//	authz.Middleware       on /mcp   RED   by mcpchain_test.go — BY SUBJECT
//	gatewayauth.Middleware on /v1    RED   BY ACCIDENT, TWICE (see below)
//	authz.Middleware       on /v1    GREEN — NOTHING IN THE REPOSITORY REDS
//
// Deleting authz.Middleware from /v1 removes membership resolution from EVERY route under it —
// 103 of them — and all 45 packages stayed green.
//
// ⚠ AND THE ROW ABOVE IT IS THE MORE INSTRUCTIVE ONE, because it looks covered and is not.
// Deleting gatewayauth.Middleware from /v1 reds two tests, and NEITHER asserts that /v1 is
// guarded:
//
//   - internal/config/mainwiring_ceilings_test.go reds because cfg.GatewayAuthSecret went from
//     two consumer sites to one. It is a CONFIG-WIRING guard; it does not know what the value
//     is for. (This is the same accident W3.33 recorded on /mcp.)
//   - internal/mountguard/authexempt_test.go reds because it locates the /v1 mount prefix by
//     finding that very call — its VACUITY branch fires, "could not find the mount prefix".
//     It lost its landmark, not its subject.
//
// Remove the cfg value or the landmark role and both reds vanish while the boundary stays open.
// **A red is not coverage until you read which test produced it, and why.**
//
// ⚠ THIS ADDS NO PERMISSION AND REMOVES NONE. Both middlewares ship on /v1 today with these
// exact predicates; nothing is added, removed or reordered. What changes is that removing one,
// or swapping which lane it exempts, becomes a deliberate edit to a named guard.
//
// Controls: ~/talyvor-queue/w334-v1chain-controls-c5j7.py.

// requiredV1Chain: middleware -> the exempt predicate it must be handed, by name.
//
// ⚠ THE PREDICATES ARE PINNED PER-MIDDLEWARE AND NOT MERELY "SOME EXEMPT", because the two are
// DIFFERENT ON PURPOSE and were the same literal until the member-sync service route 403ed in
// production (exempt.go's own header). Collapsing them in either direction is a boundary change:
// giving gatewayauth the membership predicate would exempt /v1/service/ from the SECRET — which
// exempt.go names as the one thing that must never happen, since those routes skip membership
// and the secret is all that is left.
var requiredV1Chain = map[string]string{
	"gatewayauth.Middleware": "gatewayauth.ExemptTransitProof",
	"authz.Middleware":       "gatewayauth.ExemptMembership",
}

// v1GroupOf returns the func literal passed to r.Route("/v1", …) in main.go.
func v1GroupOf(t *testing.T, file *ast.File) *ast.FuncLit {
	t.Helper()
	var found []*ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Route" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if p, err := strconv.Unquote(lit.Value); err != nil || p != "/v1" {
			return true
		}
		if fn, ok := call.Args[1].(*ast.FuncLit); ok {
			found = append(found, fn)
		}
		return true
	})
	if len(found) == 0 {
		t.Fatalf(`could not find r.Route("/v1", …) in %s. This guard protects the /v1 security `+
			`chain; a silent pass here would report a chain this file never looked at. If the `+
			`API surface moved, move the guard with it.`, mcpMainPath)
	}
	if len(found) > 1 {
		t.Fatalf(`%d separate r.Route("/v1", …) groups in %s. This guard pins ONE; checking the `+
			`first would report a healthy boundary while a second surface stood open.`,
			len(found), mcpMainPath)
	}
	return found[0]
}

// TestV1Group_CarriesBothAuthLayersWithTheirOwnPredicates pins the chain on the main API surface.
func TestV1Group_CarriesBothAuthLayersWithTheirOwnPredicates(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mcpMainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mcpMainPath, err)
	}
	group := v1GroupOf(t, file)

	seen := map[string]bool{}
	for _, stmt := range group.Body.List {
		es, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		use, ok := es.X.(*ast.CallExpr)
		if !ok || len(use.Args) != 1 {
			continue
		}
		if sel, ok := use.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Use" {
			continue
		}
		mw, ok := use.Args[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		name := qualName(mw.Fun)
		wantExempt, isSecurity := requiredV1Chain[name]
		if !isSecurity {
			continue
		}
		seen[name] = true
		if len(mw.Args) != 2 {
			t.Errorf("%s on /v1 takes %d args; this guard reads the exempt predicate as the "+
				"second. Re-derive it rather than deleting this check.", name, len(mw.Args))
			continue
		}
		if got := qualName(mw.Args[1]); got != wantExempt {
			t.Errorf("%s on /v1 is handed %q, not %q.\n"+
				"The two predicates differ ON PURPOSE and were the same literal until the "+
				"member-sync service route 403ed in production (see exempt.go). Collapsing them "+
				"is a boundary change: handing gatewayauth the MEMBERSHIP predicate would exempt "+
				"/v1/service/ from the SECRET, and those routes skip membership — the secret is "+
				"all that is left. If this swap is intended, change this table in the same commit.",
				name, got, wantExempt)
		}
	}

	for want := range requiredV1Chain {
		if !seen[want] {
			t.Errorf("the /v1 group does not use %s.\n"+
				"MEASURED (W3.34): deleting authz.Middleware here left ALL 45 PACKAGES GREEN, "+
				"and it removes membership resolution from every route under /v1 — handlers "+
				"then scope by-id queries against an empty membership set instead of the "+
				"caller's. Deleting gatewayauth.Middleware here reds only two tests and NEITHER "+
				"asserts /v1 is guarded: one is a config-wiring census noticing "+
				"cfg.GatewayAuthSecret lost a consumer, the other is a census whose mount-prefix "+
				"LANDMARK was that very call. This assertion is the one that knows why the "+
				"middleware is there.", want)
		}
	}
}
