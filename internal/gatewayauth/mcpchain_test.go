package gatewayauth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE AGENT DOOR'S AUTH CHAIN, AND THE THIRD EXEMPT PREDICATE — THE ONE WRITTEN INLINE.
//
// W3.32 pinned the /v1 exemptions as a population by calling the SHARED predicates in
// exempt.go. cmd/docs has a third, and it is not shared:
//
//	r.Group(func(r chi.Router) {
//	    mcpExempt := func(string) bool { return false }
//	    r.Use(gatewayauth.Middleware(cfg.GatewayAuthSecret, mcpExempt))
//	    r.Use(authz.Middleware(authzResolver, mcpExempt))
//	    ...
//	    r.Post("/mcp", mcpServer.HandleRPC)
//	    r.Get("/mcp/sse", mcpServer.HandleSSE)
//	})
//
// /mcp is the AGENT door — main.go's own comment calls it "no longer a public no-auth
// surface", and ask_docs reaches Lens on Docs's single service key. Because the predicate is a
// literal rather than a shared definition, W3.32's census cannot see it: that census keys on
// gatewayauth.ExemptTransitProof, which this group does not use.
//
// ⚠⚠ MEASURED 2026-08-28 (tab-c5j7, W3.33) by mutating main.go and running the whole CI
// gauntlet against a real Postgres (~/talyvor-queue/w333-mcpexempt-census-c5j7.py).
// FOUR OF FIVE MUTATIONS WERE UNGUARDED — every package green on each:
//
//	M1  mcpExempt returns TRUE          -> GREEN   both layers off, one token
//	M2  gatewayauth's exempt -> true    -> GREEN   no gateway secret on /mcp
//	M3  authz's exempt -> true          -> GREEN   no verified identity on /mcp
//	M5  authz.Middleware DELETED        -> GREEN
//	M4  gatewayauth.Middleware DELETED  -> RED     <- and for the WRONG REASON
//
// ⚠ M4 IS THE INSTRUCTIVE ONE. The only mutation anything caught was caught by
// internal/config/mainwiring_ceilings_test.go — a guard about CONFIG WIRING. It reds because
// cfg.GatewayAuthSecret went from two consumer sites to one, NOT because a security middleware
// vanished from the agent door. M5 is the same deletion one line lower and is invisible,
// because authzResolver is a local (authz.NewPGResolver(pool)) and not a cfg value. A guard
// that catches the right thing for the wrong reason protects exactly the cases that happen to
// touch its real subject, and this pair is the demonstration.
//
// ⚠ THIS ADDS NO PERMISSION AND REMOVES NONE. /mcp exempts nothing today and still exempts
// nothing. What changes is that making it exempt something becomes a deliberate edit to a
// named guard instead of a one-token change to a closure. On an authz path that is the only
// posture a session may take.
//
// Controls: ~/talyvor-queue/w333-mcpchain-controls-c5j7.py — 10 rows, 0 miss. C1-C5 are the five
// mutations above; each is RED with this file, and the command runs ONLY this test so the verdict
// cannot be borrowed from the config guard that caught C4 by accident. C6 swaps in a shared
// predicate that exempts /v1/public/ -> RED. C7 vacuity, the door renamed away -> RED. C10 a
// SECOND group mounting an /mcp route with no auth chain -> RED. C8/C9 are INVERTED and stay
// GREEN: a comment naming an exempting closure, and an unrelated middleware added to the group.
//
// ⚠ C7's FIRST FORM PASSED, AND C10 EXISTS BECAUSE OF IT. C7 originally renamed ONE of the two
// /mcp routes; the group was still found by the other, so the guard was RIGHT to stay green and
// my control was simply aimed where the instrument does not look. Re-aiming it (move BOTH routes)
// made it fire — but writing the fix surfaced a real weakness the control had not been testing:
// the walk stopped at the FIRST group mounting /mcp routes, so a SECOND door added without the
// chain would have been invisible while this guard reported the first one healthy. It now
// collects every such group and fails if there is more than one. A mis-aimed control found a
// defect anyway, by making me re-read the thing it was pointed at.

const mcpMainPath = "../../cmd/docs/main.go"

// requiredMCPMiddleware is the chain the agent door must carry, and the exempt predicate each
// is handed. "exempts nothing" is asserted on the predicate's SHAPE — a single `return false` —
// because a closure cannot be evaluated from source. That is stricter than the property it
// stands for, deliberately: any other shape is a change to the boundary and should be read by
// a human, including a switch to one of exempt.go's shared predicates.
var requiredMCPMiddleware = []string{"authz.Middleware", "gatewayauth.Middleware"}

func qualName(e ast.Expr) string {
	if s, ok := e.(*ast.SelectorExpr); ok {
		if id, ok := s.X.(*ast.Ident); ok {
			return id.Name + "." + s.Sel.Name
		}
	}
	return ""
}

// exemptsNothing reports whether fn's body is exactly `return false`.
func exemptsNothing(fn *ast.FuncLit) bool {
	if fn == nil || fn.Body == nil || len(fn.Body.List) != 1 {
		return false
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}
	id, ok := ret.Results[0].(*ast.Ident)
	return ok && id.Name == "false"
}

// TestMCPGroup_CarriesBothAuthLayersAndExemptsNothing pins the agent door's chain.
func TestMCPGroup_CarriesBothAuthLayersAndExemptsNothing(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, mcpMainPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", mcpMainPath, err)
	}

	// Find the func literal that registers the /mcp routes — identified by what it MOUNTS,
	// not by its position or by a comment, so moving the group does not silently skip this.
	var groups []*ast.FuncLit
	var mounted []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncLit)
		if !ok {
			return true
		}
		var paths []string
		for _, stmt := range fn.Body.List {
			es, ok := stmt.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := es.X.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			switch sel.Sel.Name {
			case "Get", "Post", "Put", "Patch", "Delete":
			default:
				continue
			}
			if b, ok := call.Args[0].(*ast.BasicLit); ok && b.Kind == token.STRING {
				if p, err := strconv.Unquote(b.Value); err == nil && strings.HasPrefix(p, "/mcp") {
					paths = append(paths, p)
				}
			}
		}
		if len(paths) > 0 {
			// ⚠ COLLECT, DO NOT STOP AT THE FIRST. Returning early here would check one group
			// and ignore any other that also mounts /mcp routes — so a SECOND door, added
			// without the auth chain, would be invisible while this guard reported the first
			// one healthy. Control C10 is exactly that shape.
			groups = append(groups, fn)
			mounted = append(mounted, paths...)
		}
		return true
	})

	// ── vacuity. A guard that cannot find its subject must fail loudly: with group == nil the
	// loops below iterate zero times and every assertion passes.
	if len(groups) == 0 {
		t.Fatalf("could not find the chi group that registers the /mcp routes in %s. This guard "+
			"protects the agent door; if the door moved, move the guard with it. A silent pass "+
			"here would report a chain that this file never looked at.", mcpMainPath)
	}
	if len(groups) > 1 {
		t.Fatalf("%d separate groups in %s mount /mcp routes. This guard pins ONE agent door; "+
			"with several, every one of them needs the same chain, and checking only the first "+
			"would report a healthy boundary while a second door stood open. Widen it "+
			"deliberately.", len(groups), mcpMainPath)
	}
	group := groups[0]
	sort.Strings(mounted)
	t.Logf("agent door: %s", strings.Join(mounted, ", "))

	// Local `name := func(...)` bindings in this group, so an exempt passed by name resolves.
	bound := map[string]*ast.FuncLit{}
	for _, stmt := range group.Body.List {
		as, ok := stmt.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			continue
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			continue
		}
		if fl, ok := as.Rhs[0].(*ast.FuncLit); ok {
			bound[id.Name] = fl
		}
	}

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
		if name != "gatewayauth.Middleware" && name != "authz.Middleware" {
			continue
		}
		seen[name] = true
		if len(mw.Args) != 2 {
			t.Errorf("%s on the agent door takes %d args; this guard reads the exempt predicate "+
				"as the second. Re-derive it rather than deleting this check.", name, len(mw.Args))
			continue
		}

		var fn *ast.FuncLit
		var shape string
		switch a := mw.Args[1].(type) {
		case *ast.FuncLit:
			fn, shape = a, "an inline closure"
		case *ast.Ident:
			fn, shape = bound[a.Name], "the local "+a.Name
		default:
			shape = "an expression this guard does not resolve"
		}

		if !exemptsNothing(fn) {
			t.Errorf("the exempt predicate handed to %s on the AGENT DOOR (%s) is no longer "+
				"`return false`.\n"+
				"/mcp exempts nothing today, and every path under it reaches Lens on this "+
				"instance's single service key. Measured: making this predicate return true "+
				"leaves the ENTIRE suite green (W3.33, 4 of 5 mutations unguarded), so this "+
				"assertion is the only thing standing between a one-token edit and an "+
				"unauthenticated agent surface.\n"+
				"If the exemption is intended, say so here in the same commit — this guard adds "+
				"no permission and removes none, it only makes the change visible.",
				name, shape)
		}
	}

	for _, want := range requiredMCPMiddleware {
		if !seen[want] {
			t.Errorf("the agent door's group does not use %s.\n"+
				"Both layers ship today: gatewayauth proves transit, authz resolves the verified "+
				"identity to workspace memberships. Measured (W3.33): deleting authz.Middleware "+
				"here leaves the whole suite GREEN, and deleting gatewayauth.Middleware is caught "+
				"only incidentally, by a CONFIG-WIRING guard noticing cfg.GatewayAuthSecret lost "+
				"a consumer — not by anything that knows this is a security boundary.", want)
		}
	}
}
