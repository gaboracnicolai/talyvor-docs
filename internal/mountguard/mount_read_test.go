// Package mountguard holds the MOUNT/READ AGREEMENT guard for every chi route in this repo,
// gated or not.
//
// THE CLASS. A handler reads a path param by literal name: `chi.URLParam(r, "pageID")`. The route
// it is mounted on introduces that param by literal name too: `/pages/{pageID}`. Nothing in Go
// connects the two strings. When they disagree — a mount renamed, a read renamed, a handler
// re-mounted somewhere shallower — `chi.URLParam` does not error and does not panic. IT RETURNS
// THE EMPTY STRING, and the store op underneath runs with an empty id.
//
// WHY THIS PACKAGE EXISTS AND internal/routeguard IS NOT ENOUGH — MEASURED, not assumed.
// scripts/w31-mountread-probe-8v3r.py renames a real mount, leaves its read alone, and runs the
// WHOLE suite. On two shapes, at merged main 943bec2:
//
//	A. GATED   permission DELETE /spaces/{spaceID}/permissions/{permID} -> {permIdentifier}
//	   CAUGHT by 4 tests in 3 packages, one of them STRUCTURAL: routeguard's pinned in-class
//	   route set, which fires whether or not a behavioural test happens to drive that route.
//	B. UNGATED customdomain GET /{slug} -> {slugName}
//	   CAUGHT by ONE test — TestCustomDomain_SpaceMadePrivateStopsBeingServed_RealPG, which drives
//	   that route for an unrelated reason (a private-flip regression) and notices the 404 in
//	   passing. NOTHING STRUCTURAL COVERED UNGATED MOUNTS.
//
// ⚠⚠ AND BUILDING IT FOUND A THIRD SHAPE THAT PROBE B DID NOT LOOK AT, HELD BY NOTHING AT ALL.
// The collab websocket upgrade — `r.Get("/collab/{pageID}/ws", collabHandler.ServeWS)` — is
// registered at the COMPOSITION ROOT, in cmd/docs/main.go, not by any package's Mount(r). It
// carries no enforcer, so routeguard cannot see it; it is in no package, so a walk of internal/
// cannot see it; and no behavioural test drives it. MEASURED, control C6 in
// scripts/w31-mountread-controls-7k2m.py: rename that mount and **ZERO other tests in the repo go
// red** — collab.ServeWS reads chi.URLParam(r, "pageID") on line 133 and would get "" forever.
// Probe B's "one test" was the THIN case. This one was the empty case, and it was found by this
// guard's own floor reporting `package collab: 0 routes` — see mainRoutes.
//
// That asymmetry is the whole reason for this file. routeguard's collect() only descends
// registrations of the form `.With(<x>Enf.Require(…))`; every public, service, rate-limited-only
// and plain route is outside its input set BY CONSTRUCTION. Narrow or delete that one customdomain
// test and mount/read agreement goes silent for every ungated route in the repo. Measured here:
// routeguard's input set is 19 in-class routes out of the 101 this package sees.
//
// WHAT THIS GUARD ASSERTS, in the direction that is dangerous:
//
//	every literal param a route's handler READS must be MOUNTED on that route's path.
//
// The other direction — a path mounts {x} and nobody reads it — is NOT asserted, deliberately. It
// is not a defect: `/spaces/{spaceID}/pages/{pageID}` legitimately carries {spaceID} for the
// enforcer middleware while the handler reads only {pageID}. MEASURED, and the figure is why the
// rule is one-directional: 44 of the 101 routes are in that shape, so asserting the converse would
// fire on 44 correct routes and then get switched off. The empty-string failure only exists in the
// read⊆mount direction. `go test -v` re-derives both figures rather than trusting this sentence.
//
// WHAT IT DOES NOT CLAIM. It does not know what a param MEANS, and it does not follow a param into
// the SQL — a handler can read the right name and drop it on the floor. That residue is held by the
// per-package real-Postgres tests. It also cannot see a param name assembled at run time; that is
// an ERROR here rather than a skip (see readsParams), for the same reason routeguard fatals on an
// unfoldable path: a shape the guard cannot parse is a route the guard is not covering, and silence
// there is the exact failure mode this file exists for.
package mountguard

import (
	"fmt"
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

// routeFloor is a PINNED FLOOR on the discovered population, per package. DERIVED BY RUNNING THE
// GUARD, not written from a grep — the first hand-written version of this map was wrong for eight
// of twenty-two packages, and one of those wrongs (collab: 0) was the guard failing to see a route
// at all rather than the count being off. A pin nothing compares against is decoration.
//
// WHY A FLOOR AND NOT AN EQUALITY, stated so it cannot be overread: an exact per-route pin over
// 100 routes reds on every ordinary route addition, which is how a pin becomes a rubber stamp. The
// cost is that a floor cannot see a route being REPLACED one-for-one. What it does see — and what
// actually happens when registrations move into a shape this guard does not recognise — is the
// count DROPPING. Control F2 is the record that it is not decoration.
var routeFloor = map[string]int{
	"ai":               5,
	"analytics":        3,
	"approval":         5,
	"block":            4,
	"changelog":        8,
	"collab":           1,
	"comment":          7,
	"customdomain":     7,
	"database":         10,
	"editsession":      5,
	"export":           1,
	"freshness":        2,
	"importer":         2,
	"page":             12,
	"pagelink":         3,
	"pagelock":         3,
	"permission":       6,
	"search":           1,
	"sharing":          4,
	"space":            5,
	"templatelib":      4,
	"trackintegration": 3,
}

// mainRoutes pins the routes registered DIRECTLY in cmd/docs/main.go rather than through a
// package's Mount(r), mapping each to the "<pkg> <RecvType>.<Method>" whose body carries its reads.
//
// ⚠ THIS MAP EXISTS BECAUSE THE GUARD'S FIRST RUN REPORTED package collab: 0 ROUTES. The collab
// websocket upgrade — `r.Get("/collab/{pageID}/ws", collabHandler.ServeWS)` — is the one
// parameterised route in this repo that is mounted at the composition root, so a walk of internal/
// alone cannot see it and would have covered every package EXCEPT the one route that is neither
// gated by an enforcer nor mounted by a package. It was found by the floor, which is the whole
// argument for having one.
var mainRoutes = map[string]string{
	"GET /collab/{pageID}/ws": "collab Handler.ServeWS",
}

type route struct {
	pkg, method, path, handlerKey, file string
	line                                int
}

func (r route) id() string { return r.pkg + " " + r.method + " " + r.path + " " + r.handlerKey }

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/mountguard -> repo root
}

// litString folds a route-path expression to a string: a literal, a local `base := "…"`, or
// `base + "/suffix"`. Same three shapes routeguard folds, and the same contract — ok=false means
// the caller must FAIL, never skip.
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

func pathParams(p string) map[string]bool {
	out := map[string]bool{}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			// chi allows {name:regex}; the name is everything before the first colon.
			n := strings.Trim(seg, "{}")
			if i := strings.Index(n, ":"); i >= 0 {
				n = n[:i]
			}
			out[n] = true
		}
	}
	return out
}

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

func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 || len(fd.Recv.List[0].Names) == 0 {
		return ""
	}
	return fd.Recv.List[0].Names[0].Name
}

// httpVerbs are the chi registration methods that take (path, handler).
var httpVerbs = map[string]bool{
	"Get": true, "Post": true, "Put": true, "Patch": true, "Delete": true,
	"Head": true, "Options": true, "Connect": true, "Trace": true,
	"Handle": true, "HandleFunc": true,
}

// funcIndex is one package's functions keyed by "<RecvType>.<Name>" — "" receiver for a plain
// function.
//
// ⚠ KEYED BY RECEIVER TYPE, AND THAT IS NOT COSMETIC. Keyed by bare name this guard could not tell
// page.Handler.Create from page.Store.Create; the first version reported 52 routes as ambiguous,
// and had it resolved them by picking one it would have read the STORE's body — which contains no
// chi.URLParam at all — and passed every one of them for free. This is the same trap routeguard
// records at its own collect(), met again one package over.
type funcIndex map[string]*ast.FuncDecl

// mountedPkg is one parsed package.
type mountedPkg struct {
	name   string
	funcs  funcIndex
	routes []route
}

func parsePackage(t *testing.T, fset *token.FileSet, pkg string, files []string) *mountedPkg {
	t.Helper()
	mp := &mountedPkg{name: pkg, funcs: funcIndex{}}
	var decls []*ast.FuncDecl
	var declFile []string

	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := recvTypeName(fd) + "." + fd.Name.Name
			if prev, dup := mp.funcs[key]; dup {
				t.Fatalf("%s: two declarations share the key %q (%s and %s) — the guard would read "+
					"one body and report about the other", pkg, key,
					fset.Position(prev.Pos()), fset.Position(fd.Pos()))
			}
			mp.funcs[key] = fd
			decls = append(decls, fd)
			declFile = append(declFile, p)
		}
	}

	for i, fd := range decls {
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
		mp.routes = append(mp.routes, walkRoutes(t, fset, declFile[i], pkg,
			recvName(fd), recvTypeName(fd), fd.Body, "", locals)...)
	}
	return mp
}

// walkRoutes descends a router-building body, carrying r.Route prefixes down into closures.
// selfName/selfType are the enclosing method's receiver, used to resolve `h.Handler` expressions.
func walkRoutes(t *testing.T, fset *token.FileSet, file, pkg, selfName, selfType string,
	body ast.Node, prefix string, locals map[string]string) []route {
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

		// r.Route("/prefix", func(r chi.Router){…}) — recurse with the prefix appended.
		if sel.Sel.Name == "Route" && len(call.Args) == 2 {
			sub, okS := litString(call.Args[0], locals)
			fn, okF := call.Args[1].(*ast.FuncLit)
			if okS && okF {
				out = append(out, walkRoutes(t, fset, file, pkg, selfName, selfType, fn.Body,
					prefix+strings.TrimSuffix(sub, "/"), locals)...)
				return false
			}
			// Only fatal for a chi router — some other type may have a Route method.
			if isRouterIdent(sel.X) {
				t.Fatalf("%s:%d: r.Route the guard cannot fold — every route inside this group "+
					"would be scored against the wrong path", file, fset.Position(call.Pos()).Line)
			}
			return true
		}
		// r.Group(func(r chi.Router){…}) — same prefix, new middleware stack.
		if sel.Sel.Name == "Group" && len(call.Args) == 1 {
			if fn, ok := call.Args[0].(*ast.FuncLit); ok {
				out = append(out, walkRoutes(t, fset, file, pkg, selfName, selfType, fn.Body,
					prefix, locals)...)
				return false
			}
			return true
		}

		verb := sel.Sel.Name
		pathArg, method := 0, strings.ToUpper(verb)
		switch {
		case httpVerbs[verb]:
			if verb == "Handle" || verb == "HandleFunc" {
				method = "ANY"
			}
		case verb == "Method" || verb == "MethodFunc":
			pathArg = 1
			method = "?"
			if s, ok := call.Args[0].(*ast.SelectorExpr); ok {
				method = strings.ToUpper(strings.TrimPrefix(s.Sel.Name, "Method"))
			}
		default:
			return true
		}
		if len(call.Args) < pathArg+2 {
			return true
		}
		pth, ok := litString(call.Args[pathArg], locals)
		if !ok || !strings.HasPrefix(pth, "/") {
			return true // not a route registration — some other method named Get/Post.
		}

		full := prefix + pth
		if prefix != "" && pth == "/" {
			full = prefix
		}
		key, ok := handlerKey(call.Args[len(call.Args)-1], selfName, selfType)
		if !ok {
			// A registration whose handler the guard cannot name. FATAL rather than skipped:
			// an unnamed handler is a route whose reads are unchecked, and a guard that quietly
			// drops routes reports a clean tree for a reason that has nothing to do with the tree.
			t.Fatalf("%s:%d: %s %s registers a handler expression the guard cannot resolve to a "+
				"function in this package. Teach handlerKey the shape, or pin it in mainRoutes.",
				file, fset.Position(call.Pos()).Line, method, full)
		}
		out = append(out, route{pkg: pkg, method: method, path: full, handlerKey: key,
			file: file, line: fset.Position(call.Pos()).Line})
		return true
	})
	return out
}

func isRouterIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "r"
}

// handlerKey folds a handler expression to "<RecvType>.<Method>" within the enclosing package.
//
// ⚠ THE WRAPPER CASE IS WHY THIS GUARD REACHES ROUTES routeguard CANNOT. internal/ai registers
// `r.Post("/workspaces/{wsID}/ai/write", h.limited(h.Write))` — the argument is a CALL, not a
// selector. routeguard fatals on that shape; it simply never meets it, because those five routes
// carry no enforcer. A guard that skipped them would report a clean tree while the five most
// expensive routes in the repo went unchecked.
func handlerKey(e ast.Expr, selfName, selfType string) (string, bool) {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok && id.Name == selfName && selfName != "" {
			return selfType + "." + v.Sel.Name, true
		}
		return "", false
	case *ast.Ident:
		return "." + v.Name, true // package-level func
	case *ast.CallExpr:
		// http.HandlerFunc(x), h.limited(x), middleware(x): the handler is the single argument
		// that itself resolves. Exactly one must, or the shape is ambiguous and we refuse.
		var found string
		n := 0
		for _, a := range v.Args {
			if k, ok := handlerKey(a, selfName, selfType); ok {
				found, n = k, n+1
			}
		}
		if n == 1 {
			return found, true
		}
		return "", false
	}
	return "", false
}

// readsParams returns every literal param name fd reads, following calls to functions declared in
// the SAME package to a bounded depth with cycle detection.
//
// ⚠ THE TRANSITIVE HALF IS LOAD-BEARING, MEASURED ON A REAL FILE. internal/customdomain's four
// admin handlers contain no chi.URLParam call at all: they read {wsID} through the package-level
// helper `scopeFor(r)`. A body-only guard finds ZERO reads on those four routes and passes them for
// free — the vacuous-pass shape this repo has shipped before. Control C3 is the record.
//
// ⚠ AND THE DESCENT IS RECEIVER-SCOPED, WHICH THE FIRST VERSION WAS NOT, AND IT PRODUCED A FALSE
// FINDING RATHER THAN A MISSED ONE. Descending into any same-package function whose NAME matched,
// it followed `r.URL.Query().Get("limit")` into `(*changelog.Handler).Get` — a route handler that
// reads {id} — and reported that changelog's List route "reads {id} but does not mount it". The
// method call must be on the enclosing receiver, or it is not a call into this handler's own code.
func readsParams(t *testing.T, fset *token.FileSet, funcs funcIndex, fd *ast.FuncDecl,
	seen map[*ast.FuncDecl]bool, pkg, where string) map[string]bool {
	out := map[string]bool{}
	if fd == nil || seen[fd] {
		return out
	}
	seen[fd] = true
	self, selfType := recvName(fd), recvTypeName(fd)

	ast.Inspect(fd.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := c.Fun.(type) {
		case *ast.SelectorExpr:
			// chi.URLParam(r, "name")
			if fn.Sel.Name == "URLParam" && len(c.Args) == 2 {
				if x, ok := fn.X.(*ast.Ident); ok && x.Name == "chi" {
					lit, ok := c.Args[1].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						// A non-literal param name inside a handler. The two non-literal readers
						// in this repo are MIDDLEWARE (ratelimit, permission) that take the name
						// from their mount site, and those are held by routeguard's enforcer
						// census. One reached from a handler is a read this guard cannot place,
						// and it says so rather than scoring the route clean.
						t.Errorf("%s: %s (reached from %s) reads chi.URLParam with a NON-LITERAL "+
							"name at %s — the route's mount/read agreement is unchecked.",
							pkg, fd.Name.Name, where, fset.Position(c.Pos()))
						return true
					}
					if v, err := strconv.Unquote(lit.Value); err == nil {
						out[v] = true
					}
				}
				return true
			}
			// A method call on THIS function's own receiver: h.revoke(…), h.limited(…).
			if id, ok := fn.X.(*ast.Ident); ok && self != "" && id.Name == self {
				if callee := funcs[selfType+"."+fn.Sel.Name]; callee != nil {
					for k := range readsParams(t, fset, funcs, callee, seen, pkg, where) {
						out[k] = true
					}
				}
			}
		case *ast.Ident:
			// A package-level function: scopeFor(r), domainActor(r, wsID).
			if callee := funcs["."+fn.Name]; callee != nil {
				for k := range readsParams(t, fset, funcs, callee, seen, pkg, where) {
					out[k] = true
				}
			}
		}
		return true
	})
	return out
}

// collect walks internal/ (every package) plus the direct registrations in cmd/docs/main.go.
func collect(t *testing.T) ([]route, map[string]*mountedPkg) {
	t.Helper()
	root := repoRoot(t)
	fset := token.NewFileSet()

	byDir := map[string][]string{}
	err := filepath.Walk(filepath.Join(root, "internal"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], p)
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
	}

	pkgs := map[string]*mountedPkg{}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	seen := map[string]bool{}
	var all []route
	for _, d := range dirs {
		files := byDir[d]
		sort.Strings(files)
		mp := parsePackage(t, fset, filepath.Base(d), files)
		pkgs[mp.name] = mp
		for _, r := range mp.routes {
			// De-duplicate identical registrations. internal/search registers the same route
			// twice — once wrapped in the rate limiter, once bare, in the two arms of an
			// `if h.limit != nil`. One route at run time, two in the source.
			if seen[r.id()] {
				continue
			}
			seen[r.id()] = true
			all = append(all, r)
		}
	}

	// The composition root. Only routes pinned in mainRoutes are accepted; anything else
	// parameterised registered there is a route no package Mount owns and nothing here checks.
	f, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "docs", "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	found := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		if !ok || !httpVerbs[sel.Sel.Name] || len(c.Args) < 2 {
			return true
		}
		pth, ok := litString(c.Args[0], nil)
		if !ok || !strings.HasPrefix(pth, "/") {
			return true
		}
		if len(pathParams(pth)) == 0 {
			return true // /healthz, /readyz, /metrics, /mcp — nothing to disagree about.
		}
		key := strings.ToUpper(sel.Sel.Name) + " " + pth
		target, pinned := mainRoutes[key]
		if !pinned {
			t.Errorf("cmd/docs/main.go:%d registers the parameterised route %q directly. It belongs "+
				"to no package Mount, so nothing in this guard checks its reads. Pin it in "+
				"mainRoutes.", fset.Position(c.Pos()).Line, key)
			return true
		}
		found[key] = true
		parts := strings.Fields(target)
		all = append(all, route{pkg: parts[0], method: strings.ToUpper(sel.Sel.Name), path: pth,
			handlerKey: parts[1], file: "cmd/docs/main.go", line: fset.Position(c.Pos()).Line})
		return true
	})
	for key := range mainRoutes {
		if !found[key] {
			t.Errorf("mainRoutes pins %q but cmd/docs/main.go no longer registers it — either it "+
				"moved (and this guard is now checking a route that does not exist) or the pin is "+
				"stale.", key)
		}
	}
	return all, pkgs
}

// TestNoPackageIsMountedUnderAParameterisedPrefix pins the premise every path in this guard rests
// on: a package's route paths are composed from that package alone.
//
// If cmd/docs/main.go ever mounts a handler under `r.Route("/workspaces/{wsID}", …)`, the paths
// composed here would be MISSING {wsID} while the running server supplies it — and the guard would
// report findings on correct code, or, after someone "fixed" that by loosening the check, stop
// reporting on incorrect code. The premise is checkable, so it is checked.
func TestNoPackageIsMountedUnderAParameterisedPrefix(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "cmd", "docs", "main.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	mounts := 0
	var walk func(n ast.Node, prefix string)
	walk = func(n ast.Node, prefix string) {
		ast.Inspect(n, func(m ast.Node) bool {
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			s, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if s.Sel.Name == "Route" && len(c.Args) == 2 {
				sub, okS := litString(c.Args[0], nil)
				fn, okF := c.Args[1].(*ast.FuncLit)
				if !okS || !okF {
					t.Fatalf("main.go:%d: r.Route the guard cannot fold — the mount prefix of "+
						"everything inside it is unknown", fset.Position(c.Pos()).Line)
				}
				walk(fn.Body, prefix+strings.TrimSuffix(sub, "/"))
				return false
			}
			if s.Sel.Name == "Group" && len(c.Args) == 1 {
				if fn, ok := c.Args[0].(*ast.FuncLit); ok {
					walk(fn.Body, prefix)
					return false
				}
			}
			if strings.HasPrefix(s.Sel.Name, "Mount") && len(c.Args) == 1 {
				mounts++
				if strings.Contains(prefix, "{") {
					t.Errorf("main.go:%d: %s is mounted under the parameterised prefix %q. Every "+
						"path internal/mountguard composes for that package is missing it, so the "+
						"guard is comparing reads against the wrong path set.",
						fset.Position(c.Pos()).Line, s.Sel.Name, prefix)
				}
			}
			return true
		})
	}
	walk(f, "")
	// FLOOR. Without it, replacing the Mount(r) convention would make this test pass by finding
	// nothing at all, while the premise it exists to check went unverified.
	if mounts < 20 {
		t.Errorf("found only %d Mount* calls in main.go — the route tree's shape changed and this "+
			"premise check no longer covers it", mounts)
	}
}

// TestEveryRouteMountsTheParamsItsHandlerReads is the guard.
func TestEveryRouteMountsTheParamsItsHandlerReads(t *testing.T) {
	routes, pkgs := collect(t)
	if len(routes) == 0 {
		t.Fatal("no routes found at all — the guard's input set is empty and it would pass for " +
			"free. The chi registration shape must have changed.")
	}

	fset := token.NewFileSet()
	perPkg := map[string]int{}
	for _, r := range routes {
		perPkg[r.pkg]++
		mp := pkgs[r.pkg]
		if mp == nil {
			t.Errorf("%s:%d: route in unknown package %q", r.file, r.line, r.pkg)
			continue
		}
		fd := mp.funcs[r.handlerKey]
		if fd == nil {
			t.Errorf("%s:%d: %s %s — no function %q in package %s; a route whose handler the guard "+
				"cannot read is an unchecked route", r.file, r.line, r.method, r.path,
				r.handlerKey, r.pkg)
			continue
		}
		mounted := pathParams(r.path)
		read := readsParams(t, fset, mp.funcs, fd, map[*ast.FuncDecl]bool{}, r.pkg,
			r.method+" "+r.path)

		var missing []string
		for name := range read {
			if !mounted[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf(`%s:%d: %s %s

  handler %s.%s reads chi.URLParam for %v but the route mounts no such param.
  chi.URLParam does not error on an unknown name — IT RETURNS "" — so this handler runs its
  store op with an empty id every time the route is hit.

  Either a mount was renamed and its read was not, or this handler was re-mounted somewhere
  that does not carry that param.`,
				r.file, r.line, r.method, r.path, r.pkg, r.handlerKey, braced(missing))
		}
	}

	// FLOOR, per package. See routeFloor for why a floor and not an equality.
	for pkg, want := range routeFloor {
		if got := perPkg[pkg]; got < want {
			t.Errorf("package %s: found %d routes, floor is %d. Routes do not vanish on their own — "+
				"either they moved into a registration shape this guard does not recognise (and are "+
				"now UNCHECKED), or they were deleted and this floor must be lowered deliberately.",
				pkg, got, want)
		}
	}
	for pkg := range perPkg {
		if _, known := routeFloor[pkg]; !known {
			t.Errorf("package %s registers routes but has no entry in routeFloor. Add one, so a "+
				"later refactor cannot empty it silently.", pkg)
		}
	}

	if testing.Verbose() {
		sort.Slice(routes, func(i, j int) bool { return routes[i].id() < routes[j].id() })
		converse := 0
		for _, r := range routes {
			fmt.Printf("  %-18s %-6s %-62s -> %s\n", r.pkg, r.method, r.path, r.handlerKey)
			mp := pkgs[r.pkg]
			if mp == nil || mp.funcs[r.handlerKey] == nil {
				continue
			}
			read := readsParams(t, fset, mp.funcs, mp.funcs[r.handlerKey],
				map[*ast.FuncDecl]bool{}, r.pkg, "census")
			for name := range pathParams(r.path) {
				if !read[name] {
					converse++
					break
				}
			}
		}
		// The converse count is PRINTED rather than asserted, and printing it is the point: it is
		// the evidence for why the converse direction is not a rule. Re-derive it here rather than
		// trusting the figure in this file's package comment.
		fmt.Printf("  TOTAL %d routes across %d packages; %d of them mount a param the handler "+
			"never reads (the converse, deliberately not asserted)\n",
			len(routes), len(perPkg), converse)
	}
}

func braced(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = "{" + x + "}"
	}
	return out
}
