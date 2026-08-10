// Package routeguard holds the SEC-4 L2 class guard that `.semgrep/operate-by-id-tenancy.yml`
// cannot express.
//
// THE CLASS: a route mounted under `.With(<x>Enf.Require(...))` is authorized against ONE object —
// the one whose id the enforcer's resolver reads from the URL. When such a route also carries a
// CHILD id, the handler may act on the child WITHOUT naming the parent the gate just authorized.
// The gate and the statement are then about two different objects, with only the workspace — a
// ring both are already inside — between them.
//
// THREE COPIES SHIPPED IN THREE CONSECUTIVE PRs:
//
//	#82  a86417b  changelog   GET/PATCH/DELETE/PUBLISH /…/pages/{pageID}/changelog/entries/{id}
//	                          read only {id}; store scoped by workspace_id = ANY.
//	#83  227ac2a  permission  DELETE /spaces/{spaceID}/permissions/{permID} (and the page mount)
//	                          read only {permID}.
//	#84  234548a  database    PATCH/DELETE /databases/{dbID}/rows/{rowID}, PATCH /…/views/{viewID}
//	                          read only the child id.
//
// THE SEMGREP RULE RAN ON ALL THREE PRs AND EXITED 0 ON ALL THREE. That is not a gap in its
// pattern list, it is its predicate: rule (a) excludes any statement containing
// `workspace_id = ANY`, and every one of these defects HAS that predicate — the predicate IS the
// defect. MEASURED, not read: scripts/w31-classguard-blindness.py restores each defect as it
// shipped (`git show <fix>^:<path>`, real bytes) and runs the exact CI semgrep command; 0 findings
// on all three, while a planted bare by-id write in each of those SAME files fires. The scanner
// reads the files. The rule cannot see the class.
//
// WHY THIS IS A GO TEST AND NOT ANOTHER SEMGREP RULE: the invariant is not a property of the SQL.
// `WHERE id = $1 AND page_id = $2 AND workspace_id = ANY($3)` and `WHERE id = $1 AND
// workspace_id = ANY($2)` differ by a column name whose necessity semgrep has no way to know —
// it would have to know that changelog_entries is a child of pages and that the route said so.
// The invariant is a property of the ROUTE: a handler gated on {P} that also receives a child id
// must read {P}. That is checkable, and it is checked here.
//
// WHAT THIS GUARD DOES NOT CLAIM. It asserts the handler READS the enforcer's param. It does not
// assert the param reaches the SQL — a handler could read it and drop it. That residue is held by
// the three per-package real-Postgres tests (crosspage_realpg_test.go, crossresource_realpg_test.go,
// crossdatabase_realpg_test.go), which drive mismatched pairs through the real routes. This guard
// is the one that generalises to the FOURTH copy; those are the ones that prove the three known
// copies are actually closed. Control C-DROP below is the record that this distinction is real.
package routeguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// enforcerParams is a PINNED list, not a derived one. A guard that only reads the source cannot
// see a deletion: if cmd/docs/main.go stopped constructing dbEnf, a derived-only census would
// shrink to two and stay green. TestEnforcerParamCensus asserts the source matches this list in
// BOTH directions, so adding a fourth enforcer or renaming a param fails here and forces the
// author to decide what the new gate's param is — rather than silently leaving its routes
// unchecked.
var enforcerParams = map[string]string{
	"spaceEnf": "spaceID",
	"pageEnf":  "pageID",
	"dbEnf":    "dbID",
	"blockEnf": "blockID",
}

// inClassRoutes is the pinned set of gated routes that carry a child id after the enforcer's
// param — the exact population this guard polices. Pinned for the same reason as above: a refactor
// that moves these routes elsewhere would empty the guard's input set, and a guard over an empty
// set passes. Format: "<pkg> <METHOD> <path>".
// DERIVED BY COUNTING, NOT WRITTEN BY HAND. The first version of this list was assembled from a
// grep and was missing eight of the nineteen — including every `comment` and `page` route, and the
// `approval` one that turned out to be the live defect. The guard printed the difference and that
// is how the list got fixed; a hand-maintained pin that nothing compares against is decoration.
var inClassRoutes = []string{
	"approval POST /spaces/{spaceID}/pages/{pageID}/approval/{requestID}/decide",
	"changelog DELETE /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}",
	"changelog GET /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}",
	"changelog PATCH /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}",
	"changelog POST /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}/publish",
	"comment DELETE /spaces/{spaceID}/pages/{pageID}/comments/{id}",
	"comment DELETE /spaces/{spaceID}/pages/{pageID}/comments/{id}/resolve",
	"comment POST /spaces/{spaceID}/pages/{pageID}/comments/{id}/reply",
	"comment POST /spaces/{spaceID}/pages/{pageID}/comments/{id}/resolve",
	"database DELETE /databases/{dbID}/rows/{rowID}",
	"database PATCH /databases/{dbID}/rows/{rowID}",
	"database PATCH /databases/{dbID}/views/{viewID}",
	"page GET /spaces/{spaceID}/pages/{pageID}/versions/{version}",
	"page GET /spaces/{spaceID}/pages/{pageID}/versions/{version}/diff/{other}",
	"page POST /spaces/{spaceID}/pages/{pageID}/versions/{version}/restore",
	"pagelink DELETE /pages/{pageID}/links/{issueID}",
	"permission DELETE /spaces/{spaceID}/pages/{pageID}/permissions/{permID}",
	"permission DELETE /spaces/{spaceID}/permissions/{permID}",
	"sharing DELETE /spaces/{spaceID}/pages/{pageID}/share/{id}",
}

type route struct {
	pkg, method, path, enforcerField, handler, file string
	line                                            int
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/routeguard -> repo root
}

// litString folds a route-path expression to a string. It handles the three shapes this repo
// uses — a literal, a local `base := "…"` identifier, and `base + "/suffix"`. Anything else
// returns ok=false, and every caller FAILS on that rather than skipping: a registration shape the
// guard cannot parse is a route the guard is not covering, and silence there is the whole failure
// mode this file exists for.
func litString(e ast.Expr, locals map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := locals[v.Name]
		return s, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, okL := litString(v.X, locals)
		r, okR := litString(v.Y, locals)
		return l + r, okL && okR
	}
	return "", false
}

func pathParams(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.Trim(seg, "{}"))
		}
	}
	return out
}

// recvTypeName returns the receiver type name of a method declaration ("Handler" for
// `func (h *Handler) …`), and "" for a plain function.
func recvTypeName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// collect walks internal/ and returns every route registered as `.With(<x>Enf.Require(…))`, plus
// every method body keyed by "<pkg>.<RecvType>.<Method>".
//
// ⚠ THE KEY INCLUDES THE RECEIVER TYPE AND THAT IS NOT COSMETIC. Keyed by "<pkg>.<Method>" this
// guard read the WRONG FUNCTION: internal/database declares UpdateRow on both *Handler and *Store,
// store.go parses second, and the store's body — which of course contains no chi.URLParam — landed
// on the handler's key. The guard then reported the three database routes #84 had just FIXED as
// still defective. It was caught only because those three had a known-good answer to check
// against; on a package where I had no prior it would have read as a finding.
func collect(t *testing.T) ([]route, map[string]*ast.FuncDecl) {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()
	var routes []route
	bodies := map[string]*ast.FuncDecl{}

	err := filepath.Walk(filepath.Join(root, "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", p, perr)
		}
		pkg := f.Name.Name
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := pkg + "." + recvTypeName(fd) + "." + fd.Name.Name
			if prev, dup := bodies[key]; dup {
				t.Fatalf("two declarations share the guard's body key %q (%s and %s) — the guard "+
					"would check one and report on the other", key,
					fset.Position(prev.Pos()), fset.Position(fd.Pos()))
			}
			bodies[key] = fd

			// Fold `base := "…"` locals declared anywhere in this function.
			locals := map[string]string{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				id, ok := as.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}
				if s, ok := litString(as.Rhs[0], nil); ok {
					locals[id.Name] = s
				}
				return true
			})
			routes = append(routes, walkRoutes(t, fset, p, pkg, recvTypeName(fd), fd.Body, "", locals)...)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return routes, bodies
}

// walkRoutes descends a Mount body collecting gated registrations, carrying the `r.Route("…", …)`
// PREFIX down into the closure. Without this, blocks' `r.Route("/pages/{pageID}/blocks", …)` and
// page's own group hand the guard a path of "/" — no {pageID} in it — and every route inside the
// group reads as "gated by an enforcer whose param this route never supplies". That is a guard
// artifact and it fired on four real routes before this was written.
func walkRoutes(t *testing.T, fset *token.FileSet, file, pkg, recv string, body ast.Node,
	prefix string, locals map[string]string) []route {
	var out []route
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		// r.Route("/prefix", func(r chi.Router) { … }) — recurse with the prefix appended.
		if sel.Sel.Name == "Route" && len(call.Args) == 2 {
			sub, ok := litString(call.Args[0], locals)
			if !ok {
				t.Fatalf("%s:%d: r.Route prefix is not a foldable string — every route inside "+
					"this group would be scored against the wrong path",
					file, fset.Position(call.Pos()).Line)
			}
			fn, ok := call.Args[1].(*ast.FuncLit)
			if !ok {
				t.Fatalf("%s:%d: r.Route's second argument is not a literal closure — the guard "+
					"cannot see the routes inside it", file, fset.Position(call.Pos()).Line)
			}
			out = append(out, walkRoutes(t, fset, file, pkg, recv, fn.Body,
				prefix+strings.TrimSuffix(sub, "/"), locals)...)
			return false // this subtree is handled
		}

		verb := sel.Sel.Name
		inner, ok := sel.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		innerSel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || innerSel.Sel.Name != "With" {
			return true
		}
		// Which enforcer field is in the .With(...) arguments?
		field := ""
		for _, a := range inner.Args {
			ast.Inspect(a, func(m ast.Node) bool {
				c, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				s, ok := c.Fun.(*ast.SelectorExpr)
				if !ok || s.Sel.Name != "Require" {
					return true
				}
				if fs, ok := s.X.(*ast.SelectorExpr); ok {
					field = fs.Sel.Name
				} else if id, ok := s.X.(*ast.Ident); ok {
					field = id.Name
				}
				return false
			})
		}
		if field == "" {
			return true // .With(...) carrying no .Require — not a gated route.
		}

		pathArg, method := 0, strings.ToUpper(verb)
		if verb == "Method" { // r.With(…).Method(http.MethodDelete, path, handler)
			pathArg = 1
			method = "?"
			if s, ok := call.Args[0].(*ast.SelectorExpr); ok {
				method = strings.ToUpper(strings.TrimPrefix(s.Sel.Name, "Method"))
			}
		}
		if len(call.Args) < pathArg+2 {
			t.Fatalf("%s:%d: gated registration with too few args to read a path",
				file, fset.Position(call.Pos()).Line)
		}
		pth, ok := litString(call.Args[pathArg], locals)
		if !ok {
			t.Fatalf("%s:%d: gated route path is not a foldable string — the guard cannot "+
				"see which params this route carries, so it would silently not cover it",
				file, fset.Position(call.Pos()).Line)
		}
		full := prefix + pth
		if prefix != "" && pth == "/" {
			full = prefix
		}
		// Handler expr: last arg, unwrapping http.HandlerFunc(...).
		h := call.Args[len(call.Args)-1]
		if c, ok := h.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "HandlerFunc" && len(c.Args) == 1 {
				h = c.Args[0]
			}
		}
		hname := ""
		if s, ok := h.(*ast.SelectorExpr); ok {
			hname = s.Sel.Name
		} else if id, ok := h.(*ast.Ident); ok {
			hname = id.Name
		}
		if hname == "" {
			t.Fatalf("%s:%d: gated route's handler is not a named function — the guard "+
				"cannot find a body to check", file, fset.Position(call.Pos()).Line)
		}
		out = append(out, route{pkg: pkg, method: method, path: full,
			enforcerField: field, handler: recv + "." + hname, file: file,
			line: fset.Position(call.Pos()).Line})
		return true
	})
	return out
}

// readsParam reports whether fd's body contains chi.URLParam(_, "<name>").
func readsParam(fd *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		s, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != "URLParam" || len(c.Args) != 2 {
			return true
		}
		if lit, ok := c.Args[1].(*ast.BasicLit); ok {
			if v, err := strconv.Unquote(lit.Value); err == nil && v == name {
				found = true
			}
		}
		return true
	})
	return found
}

// TestEnforcerParamCensus pins the enforcer→param binding against cmd/docs/main.go, in both
// directions. Without this, a fourth enforcer would gate routes the guard below skips entirely.
func TestEnforcerParamCensus(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "docs", "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	got := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || !strings.HasSuffix(id.Name, "Enf") {
			return true
		}
		ast.Inspect(as.Rhs[0], func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok || len(c.Args) == 0 {
				return true
			}
			s, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || !strings.Contains(s.Sel.Name, "ResolverFrom") {
				return true
			}
			p, ok := litString(c.Args[0], nil)
			if !ok {
				t.Fatalf("%s: %s's resolver param is not a literal — the guard cannot pin it",
					id.Name, s.Sel.Name)
			}
			if want, known := enforcerParams[id.Name]; !known {
				t.Errorf("cmd/docs/main.go constructs enforcer %q, which is NOT in this guard's "+
					"pinned enforcerParams map. Every route it gates is currently UNCHECKED by "+
					"TestGatedRouteHandlerReadsItsEnforcersParam. Add %q: %q and re-run.",
					id.Name, id.Name, p)
			} else if want != p {
				t.Errorf("enforcer %s resolves param %q, guard pins %q — the guard would look for "+
					"the wrong param in every route it gates", id.Name, p, want)
			}
			got[id.Name] = true
			return false
		})
		return true
	})
	for name := range enforcerParams {
		if !got[name] {
			t.Errorf("guard pins enforcer %q but cmd/docs/main.go no longer constructs it — either "+
				"its routes moved (and are now ungated) or the pin is stale. A source-derived "+
				"census cannot see this; that is why the list is pinned.", name)
		}
	}
}

// TestGatedRouteHandlerReadsItsEnforcersParam is the class guard.
//
// For every route mounted under an enforcer whose path carries a child id AFTER the enforcer's
// param, the handler must read that param. A handler that reads only the child id acts on an
// object its gate never authorized.
func TestGatedRouteHandlerReadsItsEnforcersParam(t *testing.T) {
	routes, bodies := collect(t)
	if len(routes) == 0 {
		t.Fatal("no gated routes found at all — the guard's input set is empty and it would pass " +
			"for free. The registration shape must have changed.")
	}

	var inClass []string
	for _, r := range routes {
		param, known := enforcerParams[r.enforcerField]
		if !known {
			t.Errorf("%s:%d: route gated by unknown enforcer field %q — TestEnforcerParamCensus "+
				"should have caught this; either way the route is unchecked", r.file, r.line, r.enforcerField)
			continue
		}
		params := pathParams(r.path)
		idx := -1
		for i, p := range params {
			if p == param {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("%s:%d: %s %s is gated by %s (param %q) but its path does not contain "+
				"{%s} — the enforcer's resolver reads a param this route never supplies",
				r.file, r.line, r.method, r.path, r.enforcerField, param, param)
			continue
		}
		if idx == len(params)-1 {
			continue // no child id after the gated object: not this class.
		}
		key := r.pkg + " " + r.method + " " + r.path
		inClass = append(inClass, key)

		fd, ok := bodies[r.pkg+"."+r.handler]
		if !ok {
			t.Errorf("%s:%d: cannot find the body of handler %s.%s — an in-class route whose "+
				"handler the guard cannot read is an unchecked route", r.file, r.line, r.pkg, r.handler)
			continue
		}
		if !readsParam(fd, param) {
			t.Errorf(`%s:%d: %s %s

  gated by %s, which authorizes {%s} — but %s.%s never reads chi.URLParam(r, %q).
  It acts on %v alone, so the object the gate answered about and the object the statement
  names are two different things, with only the workspace between them.

  This is the shape of #82 (changelog), #83 (permission) and #84 (database). Pass %q into the
  store and scope the statement by it; a mismatched pair must answer 404.`,
				r.file, r.line, r.method, r.path, r.enforcerField, param, r.pkg, r.handler, param,
				params[idx+1:], param)
		}
	}

	// FLOOR + PINNED SET. A guard whose population silently shrinks to zero passes. Compare the
	// discovered in-class set against the pinned one in both directions.
	sort.Strings(inClass)
	want := append([]string(nil), inClassRoutes...)
	sort.Strings(want)
	if strings.Join(inClass, "\n") != strings.Join(want, "\n") {
		t.Errorf("the set of in-class routes changed.\n  found:\n    %s\n  pinned:\n    %s\n"+
			"If a route was added, add it to inClassRoutes. If one vanished, confirm it was "+
			"deleted rather than moved out of the guard's reach.",
			strings.Join(inClass, "\n    "), strings.Join(want, "\n    "))
	}
}
