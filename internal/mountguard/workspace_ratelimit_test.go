package mountguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// workspace_ratelimit_test.go — which routes are behind the PER-WORKSPACE rate limiter, as a
// population.
//
// THE DEFECT THIS WAS WRITTEN FOR, MEASURED BEFORE IT WAS WRITTEN. Unwrapping
// `POST /v1/workspaces/{wsID}/ai/write` from `h.limited(...)` — one edit, one line — left ALL 45
// PACKAGES GREEN against a real Postgres. That route hands a prompt to an LLM provider on every
// call. `ai.Handler`'s own comment says the limiter exists because "without this a workspace can
// drive unbounded spend", and nothing in the repository asserted it is actually on the route.
//
// ⚠ IT IS A DOUBLE LOSS, AND THE SECOND HALF IS NOT OBVIOUS. `ratelimit.WorkspaceLimit` calls
// `authz.AuthorizeWorkspace` and refuses a non-member with 403 BEFORE spending a token — so the
// five AI routes are authorized TWICE, once in the middleware and once in the handler. Measured,
// not read: deleting the handler's own `AuthorizeWorkspace` leaves the route refusing, because the
// limiter refuses first. Unwrapping the limiter therefore removes a spend cap AND one of two
// authorization gates in the same edit.
//
// ⚠⚠ WHY THIS IS NOT A GREP. `h.limited(h.Write)` names no limiter: the wrapper is a same-package
// method whose BODY calls `WorkspaceLimit`. A census of spellings sees five calls to `limited` and
// has no way to know what it does; a census that resolved nothing would score all five unlimited
// and red on correct code. This walks into the wrapper's body — one level, same package — and the
// search route's `r.With(h.limit.WorkspaceLimit("wsID"))` is found by the other arm, on the
// registration's `.With` chain. Both shapes ship today and a guard that handled one would be
// silently blind to the other.
//
// ⚠⚠⚠ THIS ADDS NO LIMIT AND REMOVES NONE. It does not say what the rate SHOULD be — the numbers
// live in internal/config and are pinned there; changing one is a spend decision and not a
// session's. It records which routes are behind the limiter as it ships.

// workspaceLimited is every route registration that goes through the per-workspace limiter.
//
// ⚠ internal/search registers ITS route TWICE — `r.With(h.limit.WorkspaceLimit(...)).Get(...)`
// when a limiter is wired and a bare `r.Get(...)` when it is not, in the two arms of one `if`.
// One route at run time, two in the source. A route counts as limited when ANY of its
// registration sites is, which is why deleting the limited arm still reds: the fallback arm is
// then the only one and it is bare.
var workspaceLimited = map[string]string{
	"ai POST /workspaces/{wsID}/ai/write":         "h.limited wrapper",
	"ai POST /workspaces/{wsID}/ai/transform":     "h.limited wrapper",
	"ai POST /workspaces/{wsID}/ai/translate":     "h.limited wrapper",
	"ai POST /workspaces/{wsID}/ai/ask":           "h.limited wrapper",
	"ai POST /workspaces/{wsID}/ai/suggest-title": "h.limited wrapper",
	"search GET /workspaces/{wsID}/search":        "r.With chain (the h.limit != nil arm)",
}

func TestWorkspaceRateLimitedRoutes_ArePinnedAsAPopulation(t *testing.T) {
	routes, pkgs := collect(t)
	root := repoRoot(t)

	limited := limitedRegistrations(t, root, routes, pkgs)
	if len(limited) == 0 {
		t.Fatalf("no route was scored as workspace-rate-limited. %d ship that way; a scanner that "+
			"finds none of them would report every route as unlimited and its table as stale — "+
			"which reads exactly like a clean tree.", len(workspaceLimited))
	}

	seen := map[string]bool{}
	var undeclared []string
	for _, r := range routes {
		key := r.pkg + " " + r.method + " " + r.path
		if seen[key] {
			continue
		}
		seen[key] = true
		_, declared := workspaceLimited[key]
		switch {
		case limited[key] && !declared:
			undeclared = append(undeclared, key)
		case declared && !limited[key]:
			t.Errorf("%s is declared workspace-rate-limited (%s) and IS NOT.\n"+
				"    This route was pinned because unwrapping it left all 45 packages green. If "+
				"the limiter was removed on purpose, say so here and say what now caps the spend; "+
				"if not, it is a per-workspace spend cap that has silently stopped applying — and "+
				"on the AI routes it also removes one of the two authorization gates.",
				key, workspaceLimited[key])
		}
	}
	sort.Strings(undeclared)
	for _, key := range undeclared {
		t.Errorf("%s goes through the per-workspace rate limiter and is not in workspaceLimited. "+
			"Add it — the table is the list of routes whose spend is capped per workspace.", key)
	}
	for key := range workspaceLimited {
		if !seen[key] {
			t.Errorf("workspaceLimited lists %q, which is not a route this binary registers. A "+
				"table that outlives its routes certifies a cap on a surface that is gone.", key)
		}
	}
}

// limitedRegistrations reports which routes have at least one registration site behind the
// per-workspace limiter, by two independent shapes: the registration's `.With` chain, and a
// same-package wrapper passed as the handler argument.
func limitedRegistrations(t *testing.T, root string, routes []route, pkgs map[string]*mountedPkg) map[string]bool {
	t.Helper()
	out := map[string]bool{}

	byFile := map[string][]route{}
	for _, r := range routes {
		byFile[r.file] = append(byFile[r.file], r)
	}
	fset := token.NewFileSet()
	for file, rs := range byFile {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, file)
		}
		af, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		pkg := rs[0].pkg
		byLine := map[int]bool{}
		ast.Inspect(af, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || !httpVerbs[sel.Sel.Name] || len(c.Args) < 2 {
				return true
			}
			line := fset.Position(c.Pos()).Line
			if callsWorkspaceLimit(c.Args[len(c.Args)-1], pkgs[pkg], 0) || withChainLimits(sel.X) {
				byLine[line] = true
			}
			return true
		})
		for _, r := range rs {
			if byLine[r.line] {
				out[r.pkg+" "+r.method+" "+r.path] = true
			}
		}
	}
	return out
}

// withChainLimits walks a registration's receiver chain for `.With(<x>.WorkspaceLimit(...))`.
func withChainLimits(x ast.Expr) bool {
	for {
		wc, ok := x.(*ast.CallExpr)
		if !ok {
			return false
		}
		wsel, ok := wc.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if wsel.Sel.Name == "With" {
			for _, a := range wc.Args {
				if mentionsWorkspaceLimit(a) {
					return true
				}
			}
		}
		x = wsel.X
	}
}

// callsWorkspaceLimit resolves a handler argument. `h.limited(h.Write)` names no limiter, so the
// wrapper's own body is read — one level, same package, which is the only indirection that ships.
func callsWorkspaceLimit(arg ast.Expr, mp *mountedPkg, depth int) bool {
	if depth > 2 || mp == nil {
		return false
	}
	c, ok := arg.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fd := mp.funcs[recvTypeOfMethod(mp, sel.Sel.Name)+"."+sel.Sel.Name]
	if fd == nil || fd.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		if mentionsWorkspaceLimit(n) {
			found = true
			return false
		}
		return true
	})
	return found
}

// recvTypeOfMethod finds the receiver type that declares a method of this name in the package.
func recvTypeOfMethod(mp *mountedPkg, name string) string {
	for key := range mp.funcs {
		if i := strings.LastIndex(key, "."); i >= 0 && key[i+1:] == name {
			return key[:i]
		}
	}
	return ""
}

func mentionsWorkspaceLimit(n ast.Node) bool {
	c, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "WorkspaceLimit"
}
