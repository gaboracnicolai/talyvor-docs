package mountguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// THE BODY-CAP EXEMPTION AND THE GROUP THAT RE-CAPS IT ARE TWO POPULATIONS, AND NOTHING SAID
// THEY WERE THE SAME ONE.
//
// cmd/docs exempts a PATH PREFIX from the 4MB /v1 body cap and re-caps a ROUTE GROUP at 200MB:
//
//	r.Use(bodylimit.Middleware(cfg.MaxBodyBytes, func(p string) bool {
//	    return strings.HasPrefix(p, "/v1/import/")          // keyed on the PATH
//	}))
//	r.Group(func(r chi.Router) {
//	    r.Use(bodylimit.Middleware(cfg.MaxImportBodyBytes, nil))
//	    importerHandler.Mount(r)                            // keyed on GROUP MEMBERSHIP
//	})
//
// bodylimit.Middleware returns `next.ServeHTTP` UNWRAPPED for an exempt path, so a route that
// matches the prefix but is mounted OUTSIDE the group is exempt from the small cap and never
// reaches the large one — UNCAPPED, not re-capped. internal/bodylimit/exemptgap_test.go
// measures that consequence at the real shipped values: 200, against 413 for the same body on
// a normal /v1 route.
//
// They coincide today at exactly the importer's two routes. This pins that they still do.
// Adding `/v1/import/gdocs` to any package other than the one inside the group is the cheapest
// way to ship an unbounded request body, and it is the natural place someone would add it.
//
// ⚠ EVERYTHING IS DERIVED FROM cmd/docs/main.go, NOT RESTATED HERE. The exempt prefix, the
// /v1 mount prefix and the capped package are all read out of the file. A guard that hardcoded
// "/v1/import/" and "importer" would agree with itself forever after someone changed the
// prefix in main.go — which is the failure mode it exists to prevent, one level up.
//
// Controls: ~/talyvor-queue/w331-importcap-controls-c5j7.py — 9 rows, 0 miss. C1 is the one it
// exists for (an exempt-prefix route in a package outside the group) -> RED. C3/C4 the exemption
// changed or removed -> RED. C5 a second handler in the group -> RED. C6 the importer moved out
// -> RED. C8 vacuity, every exempt route deleted -> RED. C2 and C7 are INVERTED and stay GREEN:
// a third route added INSIDE the group is correct, and a comment naming one must not move the
// census. A census that reds on correct code gets relaxed until it reds on nothing.
//
// ⚠ IT REUSES collect() RATHER THAN WALKING THE TREE AGAIN. api_surface_census_test.go's
// header states the reason and it applies verbatim: a second walk would give this repository
// two subtly different answers to "what routes exist".

// findImportCapInvariant reads main.go and returns (routePrefix, exemptPrefix, cappedPkg).
func findImportCapInvariant(t *testing.T) (string, string, string) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "cmd", "docs", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	qual := func(e ast.Expr) string {
		if s, ok := e.(*ast.SelectorExpr); ok {
			if id, ok := s.X.(*ast.Ident); ok {
				return id.Name + "." + s.Sel.Name
			}
			return "." + s.Sel.Name
		}
		return ""
	}
	lit := func(e ast.Expr) string {
		if b, ok := e.(*ast.BasicLit); ok && b.Kind == token.STRING {
			s, err := strconv.Unquote(b.Value)
			if err == nil {
				return s
			}
		}
		return ""
	}

	// varName -> constructor package, from `x := pkg.NewFoo(...)` anywhere in main.go.
	ctorPkg := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		// Unwrap method chains: pkg.NewHandler(...).WithX(...) -> pkg.NewHandler
		e := as.Rhs[0]
		for {
			call, ok := e.(*ast.CallExpr)
			if !ok {
				break
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if base, ok := sel.X.(*ast.Ident); ok {
					ctorPkg[id.Name] = base.Name
					break
				}
				e = sel.X
				continue
			}
			break
		}
		return true
	})

	var routePrefix, exemptPrefix, cappedPkg string

	// Walk with a stack so we can name the enclosing r.Route("...") of a middleware site.
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		defer func() { stack = append(stack, n) }()

		call, ok := n.(*ast.CallExpr)
		if !ok || qual(call.Fun) != "bodylimit.Middleware" || len(call.Args) != 2 {
			return true
		}

		switch qual(call.Args[0]) {
		case "cfg.MaxBodyBytes":
			fn, ok := call.Args[1].(*ast.FuncLit)
			if !ok {
				return true // the /mcp site passes nil — not the exempting one
			}
			ast.Inspect(fn, func(m ast.Node) bool {
				c, ok := m.(*ast.CallExpr)
				if ok && qual(c.Fun) == "strings.HasPrefix" && len(c.Args) == 2 {
					if s := lit(c.Args[1]); s != "" {
						exemptPrefix = s
					}
				}
				return true
			})
			for i := len(stack) - 1; i >= 0; i-- {
				c, ok := stack[i].(*ast.CallExpr)
				if !ok || len(c.Args) != 2 {
					continue
				}
				// The receiver is a chi.Router VALUE (`r.Route(...)`), so qual() renders it
				// `r.Route` and not `.Route` — match on the method name, which is the stable
				// half regardless of what the router variable is called.
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Route" {
					if s := lit(c.Args[0]); s != "" {
						routePrefix = s
						break
					}
				}
			}
		case "cfg.MaxImportBodyBytes":
			// The enclosing r.Group(func(r chi.Router){...}) is the capped group.
			for i := len(stack) - 1; i >= 0; i-- {
				fl, ok := stack[i].(*ast.FuncLit)
				if !ok {
					continue
				}
				var mounts []string
				ast.Inspect(fl, func(m ast.Node) bool {
					c, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := c.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Mount" {
						return true
					}
					if recv, ok := sel.X.(*ast.Ident); ok {
						mounts = append(mounts, recv.Name)
					}
					return true
				})
				if len(mounts) == 1 {
					cappedPkg = ctorPkg[mounts[0]]
				} else if len(mounts) > 1 {
					t.Fatalf("the import-cap group mounts %d handlers (%s). This guard pins ONE "+
						"capped package; with several, every one of them must be checked below — "+
						"widen it deliberately rather than letting the extra mounts go unchecked.",
						len(mounts), strings.Join(mounts, ", "))
				}
				break
			}
		}
		return true
	})

	if routePrefix == "" || exemptPrefix == "" || cappedPkg == "" {
		t.Fatalf("could not read the import-cap invariant out of %s "+
			"(routePrefix=%q exemptPrefix=%q cappedPkg=%q). A guard that cannot find its own "+
			"subject must fail LOUDLY: a silent zero here would agree with any routing table. "+
			"If cmd/docs stopped exempting a prefix from the body cap, delete this file and "+
			"internal/bodylimit/exemptgap_test.go together.",
			path, routePrefix, exemptPrefix, cappedPkg)
	}
	return routePrefix, exemptPrefix, cappedPkg
}

// TestImportCap_EveryExemptRouteIsInsideTheCappedGroup is the population guard.
func TestImportCap_EveryExemptRouteIsInsideTheCappedGroup(t *testing.T) {
	routePrefix, exemptPrefix, cappedPkg := findImportCapInvariant(t)
	t.Logf("derived from cmd/docs/main.go: mount %q · exempt %q · capped package %q",
		routePrefix, exemptPrefix, cappedPkg)

	if !strings.HasPrefix(exemptPrefix, routePrefix) {
		t.Fatalf("the exempt prefix %q is not under the mount prefix %q, so stripping one from "+
			"the other below would compare paths that never meet. Re-derive both.",
			exemptPrefix, routePrefix)
	}
	// collect() reports paths relative to the /v1 mount, so compare in that space.
	relExempt := strings.TrimPrefix(exemptPrefix, routePrefix)

	routes, _ := collect(t)
	var exempt, offenders []string
	for _, r := range routes {
		if !strings.HasPrefix(r.path, relExempt) {
			continue
		}
		exempt = append(exempt, r.pkg+" "+r.method+" "+routePrefix+r.path)
		if r.pkg != cappedPkg {
			offenders = append(offenders, r.pkg+" "+r.method+" "+routePrefix+r.path)
		}
	}
	sort.Strings(exempt)
	sort.Strings(offenders)

	// ── vacuity floor. Zero matching routes is not "clean", it is a census with no subject.
	if len(exempt) == 0 {
		t.Fatalf("no route matches the exempt prefix %q, yet cmd/docs still exempts it from the "+
			"%s body cap. Either the walk stopped resolving paths (this guard is broken), or the "+
			"exempted routes are gone and the EXEMPTION should go with them — an exemption with "+
			"no routes behind it is a hole waiting for the next one.", exemptPrefix, routePrefix)
	}

	for _, o := range offenders {
		t.Errorf("route %s matches the body-cap exemption %q but is NOT in package %q, the one "+
			"mounted inside the group that re-caps at cfg.MaxImportBodyBytes.\n"+
			"It is therefore exempt from the 4MB cap and never reaches the 200MB one: UNCAPPED, "+
			"not re-capped (measured in internal/bodylimit/exemptgap_test.go).\n"+
			"Mount it inside that group, or give it a path outside %q.",
			o, exemptPrefix, cappedPkg, exemptPrefix)
	}
	if len(offenders) == 0 {
		t.Logf("%d route(s) under the exemption, all in %q: %s",
			len(exempt), cappedPkg, strings.Join(exempt, ", "))
	}
}
