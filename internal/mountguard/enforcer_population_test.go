package mountguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// enforcer_population_test.go — every route this binary registers, split by WHAT DECIDES whether
// the caller may have it, as a POPULATION.
//
// THE CLASS, and why the group-level census could not reach it. W3.34 (`0074d68`) censused the two
// middlewares on the `/v1` group — `gatewayauth` (is this caller who they say) and `authz` (which
// workspaces are they in). Those answer WHO. They do not answer whether THIS caller may perform
// THIS action on THIS object, which is `permission.Enforcer`'s job — and the enforcer is installed
// PER ROUTE, with `r.With(<x>Enf.Require(<tier>))`. A middleware that is installed per route can be
// MISSING per route, on exactly one line, and the group chain will still return 200.
//
// W3.34 stated the per-route enforcers as out of its population and named this as the next census.
//
// THE MEASUREMENT, at 0074d68: 103 routes. 71 carry an enforcer. 32 do not, AND NOT ONE OF THE 32
// IS UNGATED — 27 reach a different authority decision from inside the handler, and 5 are
// public-or-service by design. Nothing here fixes a defect; what merges is the fact that NOTHING
// PINNED THE SPLIT. A 104th route added with neither an enforcer nor an authority call joins the 32
// silently, and every existing test stays green.
//
// ⚠ THIS GUARD ADDS NO PERMISSION AND REMOVES NONE. It does not decide which tier a route should
// require, nor that a route ought to have an enforcer; both are product decisions on a shipped
// surface. It records the split as it ships and fails when the split MOVES WITHOUT BEING DECLARED.
//
// ⚠⚠ WHY THE "reaches" COLUMN IS A BARE SELECTOR NAME AND NOT A PACKAGE-QUALIFIED SYMBOL, and this
// is the most useful thing measured here. The first version of this census classified a handler's
// authority by the PACKAGE PREFIX of the call (`authz.`, `permission.`, `spaceauth.`). It reported
// SIX routes as reaching no authority at all. Three of those six were WRONG, and each was wrong
// through a DIFFERENT indirection:
//
//	importer   `h.access.AuthorizeSpaceWrite(...)` — access is a *spaceauth.Authorizer STRUCT FIELD,
//	           so the call's prefix is `h.access`, not `spaceauth`.
//	collab     `h.access.ResolveSession(...)` — access is a collab-local INTERFACE (SessionResolver),
//	           so there is no security package in the expression at all.
//	templatelib `h.access.AuthorizePageRead(...)` — same field shape as importer.
//
// All three are correctly and deliberately gated; the census accused them. That is the failure
// direction that gets a guard deleted rather than fixed, and it is lie #2 from this item's own aim
// paragraph — "a grep census is a census of spellings" — met three times in one run. Matching the
// SELECTOR alone survives all three shapes. The cost is stated rather than hidden: a selector match
// is looser than a symbol match, so this column pins THAT THE CALL IS STILL THERE, not that it is
// the same package's. It is a staleness pin on a declared row, not an authorization proof.
//
// ⚠⚠⚠ THE FIVE FREE-PASS ROWS ARE NOT A FREE PASS, and this is the guard's teeth. A row may declare
// no authority call ONLY if its route is registered by a function whose NAME declares the exemption
// — `PublicHandler`, `MountPublic`, `MountService`. Every ordinary route in this repository is
// registered by `Mount`. So a route added to `Mount` cannot claim public-by-design: it must carry an
// enforcer or name an authority call. Measured, not assumed: exemptRegistrars is checked against the
// enclosing FuncDecl of the registration's own source position.

// enfRow is one route that carries no route-level permission enforcer.
//
// reaches: the selector the handler must still transitively reach, or "" for a free-pass row.
// registrar: required only for a free-pass row — the registration function that declares the exemption.
type enfRow struct {
	reaches   string
	registrar string
	why       string
}

// exemptRegistrars are the registration functions whose NAME declares that the routes they register
// are not behind membership. A free-pass row must name one of these AND the route's own enclosing
// registration function must be it.
var exemptRegistrars = map[string]bool{
	"Handler.PublicHandler": true, // custom-domain read-only renderer, off the /v1 tree entirely
	"Handler.MountPublic":   true, // /v1/public/s/{token} — authenticated BY the share token
	"Syncer.MountService":   true, // /v1/service/… — gatewayauth secret, authz-exempt by design
}

// noEnforcer is the declared population of routes with no `.With(<x>Enf.Require(...))`.
// Keyed "<PKG> <METHOD> <PATH>" with the path exactly as the shared route walk folds it.
var noEnforcer = map[string]enfRow{
	// ── workspace-membership authority, decided inside the handler ────────────────────────
	"ai POST /workspaces/{wsID}/ai/write":         {reaches: "AuthorizeWorkspace", why: "workspace-scoped; no page or space object in the path"},
	"ai POST /workspaces/{wsID}/ai/transform":     {reaches: "AuthorizeWorkspace", why: "workspace-scoped"},
	"ai POST /workspaces/{wsID}/ai/translate":     {reaches: "AuthorizeWorkspace", why: "workspace-scoped"},
	"ai POST /workspaces/{wsID}/ai/ask":           {reaches: "AuthorizeWorkspace", why: "workspace-scoped"},
	"ai POST /workspaces/{wsID}/ai/suggest-title": {reaches: "AuthorizeWorkspace", why: "workspace-scoped"},

	"analytics GET /workspaces/{wsID}/analytics/pages":                      {reaches: "AuthorizeWorkspace", why: "workspace roll-up; the by-space roll-up next to it IS enforcer-gated"},
	"approval GET /workspaces/{wsID}/approvals/pending":                     {reaches: "AuthorizeWorkspace", why: "workspace-wide queue"},
	"changelog GET /workspaces/{wsID}/changelog/feed":                       {reaches: "AuthorizeWorkspace", why: "workspace-wide feed"},
	"freshness GET /workspaces/{wsID}/freshness":                            {reaches: "AuthorizeWorkspace", why: "workspace roll-up"},
	"page GET /workspaces/{wsID}/pages/search":                              {reaches: "AuthorizeWorkspace", why: "workspace-scoped query, no single page object"},
	"page GET /workspaces/{wsID}/pages/stale":                               {reaches: "AuthorizeWorkspace", why: "workspace-scoped query"},
	"search GET /workspaces/{wsID}/search":                                  {reaches: "AuthorizeWorkspace", why: "workspace-scoped query; the .With here is the RATE LIMITER, not a gate"},
	"space GET /workspaces/{wsID}/spaces":                                   {reaches: "AuthorizeWorkspace", why: "lists the spaces themselves — no space id yet to enforce against"},
	"space POST /spaces":                                                    {reaches: "AuthorizeWorkspace", why: "creates the space; the workspace arrives in the body and is authorized there"},
	"trackintegration GET /workspaces/{wsID}/track/search":                  {reaches: "AuthorizeWorkspace", why: "workspace-scoped proxy to Track"},
	"trackintegration GET /workspaces/{wsID}/track/issues/{issueID}":        {reaches: "AuthorizeWorkspace", why: "the issue is Track's object, not a docs resource"},
	"templatelib GET /workspaces/{wsID}/template-library":                   {reaches: "AuthorizeWorkspace", why: "workspace-scoped list"},
	"templatelib DELETE /workspaces/{wsID}/template-library/{templateID}":   {reaches: "AuthorizeWorkspace", why: "template is a workspace-level object"},
	"templatelib POST /workspaces/{wsID}/template-library/{templateID}/use": {reaches: "AuthorizeSpaceWrite", why: "writes into the TARGET space, which arrives in the body"},
	"templatelib POST /workspaces/{wsID}/template-library/from-page":        {reaches: "AuthorizePageRead", why: "reads the SOURCE page, which arrives in the body"},

	"customdomain GET /workspaces/{wsID}/custom-domains":              {reaches: "WorkspaceIDs", why: "scoped to the caller's workspace set"},
	"customdomain POST /workspaces/{wsID}/custom-domains":             {reaches: "WorkspaceIDs", why: "scoped to the caller's workspace set"},
	"customdomain POST /workspaces/{wsID}/custom-domains/{id}/verify": {reaches: "WorkspaceIDs", why: "scoped to the caller's workspace set"},
	"customdomain DELETE /workspaces/{wsID}/custom-domains/{id}":      {reaches: "WorkspaceIDs", why: "scoped to the caller's workspace set"},

	// ── object authority, decided inside the handler because the object is not in the URL ──
	"importer POST /import/confluence": {reaches: "AuthorizeSpaceWrite", why: "space_id is in the MULTIPART FORM, so no resolver can read it from the path"},
	"importer POST /import/notion":     {reaches: "AuthorizeSpaceWrite", why: "space_id is in the MULTIPART FORM"},
	"collab GET /collab/{pageID}/ws":   {reaches: "ResolveSession", why: "registered at the composition root; resolves in-scope + actor + tier before the WS upgrade"},

	// ── free pass: no authority call, and the REGISTRAR is what earns it ───────────────────
	"customdomain GET /":                                           {registrar: "Handler.PublicHandler", why: "custom-domain public renderer, off the /v1 tree"},
	"customdomain GET /search":                                     {registrar: "Handler.PublicHandler", why: "custom-domain public renderer"},
	"customdomain GET /{slug}":                                     {registrar: "Handler.PublicHandler", why: "custom-domain public renderer"},
	"sharing GET /public/s/{token}":                                {registrar: "Handler.MountPublic", why: "authenticated BY the share token, not by membership"},
	"trackintegration POST /service/workspaces/{wsID}/member-sync": {registrar: "Syncer.MountService", why: "gatewayauth secret; authz-exempt so a brand-new identity can be synced"},
}

// The split as it ships. A change to any of these three numbers is a change to the authorization
// surface and has to be declared here, in this file, next to the row that moved.
const (
	wantRoutes   = 103
	wantGated    = 71
	wantDeclared = 32
)

func TestRoutePermissionEnforcers_ArePinnedAsAPopulation(t *testing.T) {
	routes, pkgs := collect(t)
	root := repoRoot(t)

	// VACUITY. A walk that finds nothing agrees with every table.
	if len(routes) < 50 {
		t.Fatalf("the shared route walk returned %d routes — far below this repository's surface. "+
			"Every assertion below would pass against an empty tree.", len(routes))
	}

	gated, registrar := enforcerChains(t, root, routes)
	if len(gated) == 0 {
		t.Fatalf("not one route was scored as enforcer-gated. %d routes ship with "+
			"`r.With(<x>Enf.Require(...))`; a scanner that finds none of them would pass every "+
			"route through the declared table.", wantGated)
	}

	var undeclared, stale []string
	seen := map[string]bool{}
	for _, r := range routes {
		key := r.pkg + " " + r.method + " " + r.path
		seen[key] = true
		row, declared := noEnforcer[key]
		switch {
		case gated[key] && declared:
			t.Errorf("%s carries a permission enforcer AND is listed as having none. One of the two "+
				"is now false; the table row says %q.", key, row.why)
		case gated[key]:
			// enforcer-gated — nothing further to assert here. Which TIER it requires is a
			// product decision and deliberately not this guard's business.
		case declared:
			checkRow(t, pkgs, registrar, r, key, row)
		default:
			undeclared = append(undeclared, key)
		}
	}
	for key := range noEnforcer {
		if !seen[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)

	for _, key := range undeclared {
		t.Errorf("%s registers a route with NO permission enforcer and no entry in noEnforcer.\n"+
			"    The group chain on /v1 proves WHO the caller is and which workspaces they are in. "+
			"It does not decide whether they may do THIS to THIS object.\n"+
			"    Either mount it with `r.With(<x>Enf.Require(<tier>))`, or add a row saying which "+
			"authority its handler reaches — and if the answer is 'none', that is the finding, not "+
			"the row.", key)
	}
	for _, key := range stale {
		t.Errorf("noEnforcer lists %q, which is not a route this binary registers. A table that "+
			"outlives its routes certifies a surface that no longer exists.", key)
	}

	if len(routes) != wantRoutes || len(gated) != wantGated || len(noEnforcer) != wantDeclared {
		t.Errorf("the split moved: routes=%d (want %d), enforcer-gated=%d (want %d), declared=%d (want %d).\n"+
			"    This is the authorization surface of the binary. Update the constants and the row "+
			"that moved together, in the same commit, or the next reader cannot tell which changed.",
			len(routes), wantRoutes, len(gated), wantGated, len(noEnforcer), wantDeclared)
	}
}

// checkRow holds a declared row to its own claim.
func checkRow(t *testing.T, pkgs map[string]*mountedPkg, registrar map[string]string, r route, key string, row enfRow) {
	t.Helper()
	if (row.reaches == "") == (row.registrar == "") {
		t.Errorf("%s: a row must name EXACTLY ONE of reaches / registrar. A row with neither is an "+
			"unexamined route wearing a comment; a row with both hides which one is load-bearing.", key)
		return
	}
	if row.why == "" {
		t.Errorf("%s: no reason given. The reason is the only part of this table a human reads.", key)
	}
	if row.registrar != "" {
		if !exemptRegistrars[row.registrar] {
			t.Errorf("%s claims exemption via %q, which is not a declared exempt registrar. Add it to "+
				"exemptRegistrars deliberately — that map is the list of ways a route may skip "+
				"membership.", key, row.registrar)
			return
		}
		if got := registrar[key]; got != row.registrar {
			t.Errorf("%s claims exemption via %q but is registered by %q. A free pass is earned by "+
				"WHERE the route is registered, not by the row asserting it.", key, row.registrar, got)
		}
		return
	}
	mp := pkgs[r.pkg]
	if mp == nil {
		t.Errorf("%s: no parsed package %q to resolve %s in.", key, r.pkg, r.handlerKey)
		return
	}
	fd := mp.funcs[r.handlerKey]
	if fd == nil {
		t.Errorf("%s: handler %s not found in package %s. The row cannot be checked, and an "+
			"uncheckable row is not a passing one.", key, r.handlerKey, r.pkg)
		return
	}
	if !reachesSelector(mp.funcs, fd, row.reaches, map[*ast.FuncDecl]bool{}, 0) {
		t.Errorf("%s no longer reaches %s(). The row says its authority is %q — %s. Either the "+
			"authority moved (update the row) or it is GONE, and this route is now decided by "+
			"membership alone.", key, row.reaches, row.reaches, row.why)
	}
}

// reachesSelector reports whether fd, or any same-package function it calls, contains a call whose
// selector is name. Receiver methods and package-level funcs are followed; depth is bounded.
//
// ⚠ SELECTOR, NOT SYMBOL — see the file header. The three shapes this has to survive are a call on
// a struct field of a security type, a call on a package-local interface, and a plain qualified
// call. Only the selector is common to all three.
func reachesSelector(funcs funcIndex, fd *ast.FuncDecl, name string, seen map[*ast.FuncDecl]bool, depth int) bool {
	if fd == nil || fd.Body == nil || seen[fd] || depth > 6 {
		return false
	}
	seen[fd] = true
	self, selfType := recvName(fd), recvTypeName(fd)
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := c.Fun.(type) {
		case *ast.SelectorExpr:
			if fn.Sel.Name == name {
				found = true
				return false
			}
			if id, ok := fn.X.(*ast.Ident); ok && id.Name == self && self != "" {
				if reachesSelector(funcs, funcs[selfType+"."+fn.Sel.Name], name, seen, depth+1) {
					found = true
					return false
				}
			}
		case *ast.Ident:
			if reachesSelector(funcs, funcs["."+fn.Name], name, seen, depth+1) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// enforcerChains re-reads each registration at its own source position and reports which routes
// carry a permission enforcer, and which function registered each one.
//
// ⚠ IT KEYS ON THE POSITION THE SHARED WALK RECORDED rather than re-deriving the route set. A second
// walk would give this repository two answers to "what routes exist", which is the failure
// api_surface_census_test.go was written to avoid one file over.
//
// ⚠⚠ AN ENFORCER IS `<recv>.<field>.Require(<one arg>)` WHERE THE FIELD IS DECLARED
// `*permission.Enforcer` — not any method named Require. The rate limiter also mounts with
// `r.With(...)` on two of these routes (`h.limit.WorkspaceLimit("wsID")`), and a check that counted
// any `.With` as a gate would score both as authorized.
func enforcerChains(t *testing.T, root string, routes []route) (map[string]bool, map[string]string) {
	t.Helper()
	gated := map[string]bool{}
	registrar := map[string]string{}

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
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("route file %s: %v — a registration the guard cannot re-read is not a "+
				"registration it may skip", file, err)
		}
		af, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		fields := structFields(af)
		byLine := map[int][]string{} // line -> enforcer expressions on that registration's chain
		enclose := map[int]string{}  // line -> enclosing FuncDecl
		ast.Inspect(af, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Use" {
				for _, a := range c.Args {
					if e := enforcerArg(a, fields, ""); e != "" {
						t.Errorf("%s:%d installs a permission enforcer with r.Use(%s). This guard "+
							"reads only the per-registration `.With(...)` chain, so every route in "+
							"that group would be scored ungated. Teach it group scope before this "+
							"lands.", file, fset.Position(c.Pos()).Line, e)
					}
				}
				return true
			}
			line := fset.Position(c.Pos()).Line
			for x := sel.X; ; {
				wc, ok := x.(*ast.CallExpr)
				if !ok {
					break
				}
				wsel, ok := wc.Fun.(*ast.SelectorExpr)
				if !ok {
					break
				}
				if wsel.Sel.Name == "With" {
					recv := ""
					for _, d := range af.Decls {
						fd, ok := d.(*ast.FuncDecl)
						if ok && fd.Body != nil && line >= fset.Position(fd.Pos()).Line &&
							line <= fset.Position(fd.End()).Line {
							recv = recvTypeName(fd)
						}
					}
					for _, a := range wc.Args {
						if e := enforcerArg(a, fields, recv); e != "" {
							byLine[line] = append(byLine[line], e)
						}
					}
				}
				x = wsel.X
			}
			return true
		})
		for _, r := range rs {
			key := r.pkg + " " + r.method + " " + r.path
			if len(byLine[r.line]) > 0 {
				gated[key] = true
			}
			if _, ok := enclose[r.line]; !ok {
				enclose[r.line] = enclosingFunc(fset, af, r.line)
			}
			registrar[key] = enclose[r.line]
		}
	}
	return gated, registrar
}

// enforcerArg returns the expression text when e is `<x>.<field>.Require(<one arg>)` and <field> is
// declared *permission.Enforcer on some struct in this file; else "".
func enforcerArg(e ast.Expr, fields map[string]map[string]string, recvType string) string {
	c, ok := e.(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return ""
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Require" {
		return ""
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	field := inner.Sel.Name
	for ty, fs := range fields {
		if recvType != "" && ty != recvType {
			continue
		}
		if got, ok := fs[field]; ok && (got == "permission.Enforcer" || got == "Enforcer") {
			return field + ".Require(...)"
		}
	}
	return ""
}

// structFields indexes every struct in one file: type name -> field name -> declared type.
func structFields(af *ast.File) map[string]map[string]string {
	out := map[string]map[string]string{}
	ast.Inspect(af, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		m := map[string]string{}
		for _, f := range st.Fields.List {
			ty := f.Type
			if s, ok := ty.(*ast.StarExpr); ok {
				ty = s.X
			}
			name := typeString(ty)
			for _, nm := range f.Names {
				m[nm.Name] = name
			}
		}
		out[ts.Name.Name] = m
		return true
	})
	return out
}

func typeString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return typeString(v.X) + "." + v.Sel.Name
	}
	return ""
}

func enclosingFunc(fset *token.FileSet, af *ast.File, line int) string {
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if line >= fset.Position(fd.Pos()).Line && line <= fset.Position(fd.End()).Line {
			return strings.TrimPrefix(recvTypeName(fd)+"."+fd.Name.Name, ".")
		}
	}
	return ""
}
