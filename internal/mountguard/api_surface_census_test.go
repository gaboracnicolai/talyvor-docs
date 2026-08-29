package mountguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// api_surface_census_test.go — every HTTP request the SPA issues, against every route this binary
// registers, MATCHED ON METHOD AND PATH, in both directions.
//
// It lives in mountguard because mountguard already owns the AST walk that resolves a chi route's
// path through nested `Route()` prefixes and `Mount(r)` methods (`collect`). Re-deriving that walk
// in a new package would give this repository two subtly different answers to "what routes exist",
// which is the failure the walk was written to prevent.
//
// THE RESULTS, MEASURED AT 97ba255 (100 routes under /v1, 91 distinct SPA verb+path pairs):
//
//	(a) SPA → server: ZERO. Every request the SPA makes reaches a registered route with a
//	    matching method. A mistyped path typechecks perfectly and fails only at runtime, so
//	    nothing else here could catch it.
//
//	(b) server → SPA: 9 of 100 have no SPA caller, pinned below WITH WHAT DOES REACH THEM.
//
// ⚠ THE FINDING IS FOUR OF THOSE NINE: `internal/block` IS REACHABLE BY NOTHING THAT SHIPS.
//
//	GET    /v1/pages/{pageID}/blocks     PATCH  /v1/blocks/{blockID}
//	POST   /v1/pages/{pageID}/blocks     DELETE /v1/blocks/{blockID}
//
// 361 lines of production Go (handler.go 140 + store.go 221), a `blocks` table, and four routes
// each behind a permission enforcer. Measured, not inferred:
//   - No client in the estate calls any of the four — not this SPA, not talyvor-lens, -track,
//     -code, -suite, -research, and not this repo's own README or compose files.
//   - `internal/block` is imported by exactly ONE production file, `cmd/docs/main.go`, and only to
//     construct the handler and Mount it.
//   - EVERY `INSERT INTO blocks` / `UPDATE blocks` / `DELETE FROM blocks` in the repository is in
//     `internal/block/store.go`, reached only through that handler.
//
// So the `blocks` table can only ever be empty in production. The handler's own comment says
// "Blocks ARE the page's content: read=View, mutation=Edit" — but the SPA stores content as a JSON
// document on `pages` and its editor's "blocks" (`components/editor/blocks/*`) are client-side
// nodes in that document, not rows in this table. Two designs shipped and only one is wired.
//
// ⚠ NOTHING IS CHANGED. Deleting a package, its table and four permission-gated routes is a product
// decision, and so is wiring an editor to them. This file makes either one announce itself.
//
// ⚠ WHERE THE EVIDENCE STOPS. The SPA half is measured here, in CI, every run. The "anywhere in the
// estate" half was measured ONCE, out of CI — CI checks out only this repository and cannot
// re-verify it — and it is PATH-level, not verb-level. That distinction is not academic: the
// changelog row below is in the table precisely because the SPA PATCHes and DELETEs that path and
// never GETs it, and a path-level search finds `changelog.ts` and would have called it reached.
//
// Controls: ~/talyvor-queue/w327-docs-surface-controls-x7p2.py — 9/9 armed, every mutation
// compile-checked first. Q1 a call to a path no route serves → red · Q2 the right path with an
// unserved method → red, reported DISTINCTLY as the 405 class · Q3 the path-helper form broken →
// red (five requests vanish, so the resolver is load-bearing) · Q4 a UI wired to a table entry →
// red · Q5 a new uncalled route → red · Q6 a bogus /v1 path in a frontend COMMENT → GREEN ·
// Q7 a handler Mounted outside the /v1 block → red · Q8 frontend/src/api removed → the floor reds ·
// Q9 unmutated → GREEN.
//
// ⚠ THE PORT'S OWN BUG, KEPT HERE BECAUSE THE NEXT PERSON WILL WRITE IT. Go's
// `Regexp.ReplaceAllString` EXPANDS `$` in the replacement. The helper bodies substituted above
// are full of `${spaceID}`, so a template-based replacement quietly substituted the empty string
// for every parameter and produced `/v1/spaces//pages//edit-session` — THREE phantom 404s in
// direction (a) and THREE phantom unreached routes in (b), from one call. `ReplaceAllStringFunc`
// does no expansion. The tests caught it by going red, which is the only reason it is a footnote.

// ── (0) the /v1 prefix is a MEASURED fact, not an assumption ────────────────────────────────────
//
// `collect` returns each route's path RELATIVE to the package's Mount. Turning that into the path a
// client types requires knowing where main.go mounts each handler. Assuming "/v1" is exactly the
// kind of unstated premise this repo's guards exist to catch — and it is FALSE for one handler:
// customdomain.PublicHandler() is mounted at the ROOT of a host-based DomainRouter, so its three
// routes ("/", "/search", "/{slug}") are not /v1 routes at all. Prefixing them anyway invented
// `GET /v1` and `GET /v1/{slug}` and put two routes that do not exist into the census.

const v1Prefix = "/v1"

func TestEveryHandlerIsMountedUnderV1ExceptTheCustomDomainPublicRouter(t *testing.T) {
	root := repoRoot(t)
	main := filepath.Join(root, "cmd", "docs", "main.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, main, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", main, err)
	}

	inV1 := map[string]bool{}  // "spaceHandler.Mount"
	outV1 := map[string]bool{} // same, but registered outside the /v1 block

	var walk func(n ast.Node, underV1 bool)
	walk = func(n ast.Node, underV1 bool) {
		ast.Inspect(n, func(nd ast.Node) bool {
			call, ok := nd.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// r.Route("/v1", func(r chi.Router){ … })
			if sel.Sel.Name == "Route" && len(call.Args) == 2 {
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					p, _ := strconv.Unquote(lit.Value)
					if fl, ok := call.Args[1].(*ast.FuncLit); ok {
						walk(fl.Body, underV1 || p == v1Prefix)
						return false
					}
				}
			}
			if sel.Sel.Name == "Mount" || sel.Sel.Name == "MountPublic" {
				if recv, ok := sel.X.(*ast.Ident); ok {
					key := recv.Name + "." + sel.Sel.Name
					if underV1 {
						inV1[key] = true
					} else {
						outV1[key] = true
					}
				}
			}
			return true
		})
	}
	walk(f, false)

	// ⚠ FLOOR. If the walk stops finding Mount calls it reports "everything is under /v1" for the
	// same reason a correct tree does. 22 were measured at 97ba255.
	if len(inV1) < 18 {
		t.Fatalf("found only %d handler Mount calls inside the %q block (22 at 97ba255) — the walk "+
			"has gone blind, or the composition root was restructured. Either way the /v1 prefix "+
			"this census applies is no longer a measured fact.", len(inV1), v1Prefix)
	}
	if len(outV1) > 0 {
		var got []string
		for k := range outV1 {
			got = append(got, k)
		}
		sort.Strings(got)
		t.Errorf("%d handler(s) are Mounted OUTSIDE the %q block: %s\n\n"+
			"Every route this census reads from mountguard's collect() is prefixed with %q. A "+
			"handler mounted elsewhere gets the wrong path silently, in the direction of "+
			"\"that route does not exist\". Give it its own prefix here before adding it.",
			len(outV1), v1Prefix, strings.Join(got, ", "), v1Prefix)
	}
}

// customDomainPublicRoutes are the ONLY routes collect() returns that are not under /v1: they are
// registered by customdomain.PublicHandler(), which cmd/docs/main.go hands to DomainRouter as the
// handler for a verified custom-domain Host. They are excluded from the /v1 census by path, and
// the test above is what keeps "excluded by path" honest — it fails if any HANDLER moves out.
var customDomainPublicRoutes = map[string]bool{
	"GET /":       true,
	"GET /search": true,
	"GET /{slug}": true,
}

func serverRoutes(t *testing.T) (map[string]bool, map[string][]string) {
	t.Helper()
	pairs := map[string]bool{}
	byPath := map[string][]string{}
	routes, _ := collect(t)
	for _, r := range routes {
		key := r.method + " " + r.path
		if r.pkg == "customdomain" && customDomainPublicRoutes[key] {
			continue
		}
		p := normAPIPath(v1Prefix + r.path)
		pairs[r.method+" "+p] = true
		byPath[p] = append(byPath[p], r.method)
	}
	return pairs, byPath
}

// ── the SPA-side scanner ────────────────────────────────────────────────────────────────────────
//
// ⚠ THIS REPO'S SPA DOES NOT WRITE PATHS AS SELF-CONTAINED LITERALS, and a scanner that assumes it
// does gets THREE separate answers wrong, each in a reportable direction:
//
//	editsession.ts   `const base = (s,p) => `/v1/spaces/${s}/pages/${p}/edit-session``, then
//	                 `${base(s,p)}/heartbeat` AND a bare `apiRequest(base(s,p), {method:"POST"})`.
//	                 Literal-only: five real requests invisible, five routes falsely "unreached".
//	database.ts:97   `/v1/databases/${dbID}/rows${suffix}` — a PREBUILT QUERY STRING glued on with
//	                 no `/`. Treating it as a segment invents `/v1/databases/{}/rows{}` and reports
//	                 a route the server does not serve. It was the only direction-(a) hit here.
//	templates.ts     `${qs({\n … \n})}` spanning lines — a non-greedy `\$\{qs\(.*?\)\}` stops at the
//	                 first `}` INSIDE the object and leaves `{})}` glued to the path.
//
// So: brace-BALANCED interpolation handling, helper resolution, and a `{}` that does not follow a
// `/` is a suffix rather than a segment.

var (
	reAPILit = regexp.MustCompile("`[^`]*`|\"/v1[^\"]*\"|'/v1[^']*'")
	// a module that ISSUES a request: `apiRequest<T>(`, `apiRequest(`, a bare `fetch(`, or a
	// `new WebSocket(` — the upgrade is served by a chi Get and `hooks/useCollab.ts` reaches
	// `/v1/collab/{}/ws` with nothing else. Leaving it out cost exactly one route its only caller.
	reIssuesRequest = regexp.MustCompile(`apiRequest\s*[<(]|\bfetch\s*\(|new\s+WebSocket\s*\(`)
	reAPIMethod     = regexp.MustCompile(`method\s*:\s*["']([A-Za-z]+)["']`)
	reAPIParam      = regexp.MustCompile(`\{[^}]*\}`)
	reAPISuffix     = regexp.MustCompile(`([^/])\{\}`)
	reAPIHelper     = regexp.MustCompile("const\\s+([A-Za-z_$][\\w$]*)\\s*=\\s*\\([^)]*\\)\\s*=>\\s*\\n?\\s*`([^`]*)`")
)

type spaReq struct{ Verb, Path, File string }

// stripInterp replaces every ${…} with {} using BRACE BALANCING, dropping the ones that build a
// query string rather than a path segment.
func stripInterp(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '{' {
			depth, j := 1, i+2
			for j < len(s) && depth > 0 {
				switch s[j] {
				case '{':
					depth++
				case '}':
					depth--
				}
				j++
			}
			inner := strings.TrimSpace(s[i+2 : max(j-1, i+2)])
			if !strings.HasPrefix(inner, "qs(") && inner != "q" && inner != "query" {
				b.WriteString("{}")
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// normAPIPath folds an SPA template literal and a chi pattern onto one shape.
func normAPIPath(s string) string {
	s = stripInterp(s)
	s = reAPIParam.ReplaceAllString(s, "{}")
	for {
		n := reAPISuffix.ReplaceAllString(s, "$1")
		if n == s {
			break
		}
		s = n
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 1 && strings.HasSuffix(s, "/") {
		s = s[:len(s)-1]
	}
	return s
}

func stripTSComments(src string) string {
	var b strings.Builder
	mode := byte(0)
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch mode {
		case 0:
			if c == '/' && i+1 < len(src) && src[i+1] == '/' {
				mode, i = 'L', i+1
				continue
			}
			if c == '/' && i+1 < len(src) && src[i+1] == '*' {
				mode, i = 'B', i+1
				continue
			}
			if c == '`' || c == '"' || c == '\'' {
				mode = c
			}
			b.WriteByte(c)
		case 'L':
			if c == '\n' {
				mode = 0
				b.WriteByte(c)
			}
		case 'B':
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				mode, i = 0, i+1
				b.WriteByte(' ')
				continue
			}
			if c == '\n' {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(src[i+1])
				i++
				continue
			}
			if c == mode {
				mode = 0
			}
		}
	}
	return b.String()
}

func spaRequests(t *testing.T) []spaReq {
	t.Helper()
	root := filepath.Join(repoRoot(t), "frontend", "src")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("frontend/src not found at %s: %v", root, err)
	}
	var out []spaReq
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		base := filepath.Base(p)
		// ⚠ TEST FILES ARE NOT THE CLIENT. Fixtures here spell literal ids ("/v1/databases/db-1"),
		// which are not routes and would each become a phantom SPA request.
		if !(strings.HasSuffix(p, ".ts") || strings.HasSuffix(p, ".tsx")) ||
			strings.Contains(base, ".test.") || strings.HasSuffix(base, ".d.ts") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		src := stripTSComments(string(raw))
		rel, _ := filepath.Rel(root, p)

		// ⚠ AND NEITHER IS A MODULE THAT ANALYSES THE CLIENT — the same reasoning as the test-file
		// exclusion above, one step further out. `reAPILit` matches ANY `"/v1…"` string, so
		// `api/requestSurface.ts` (which parses this SPA's own request sites for
		// request-field.census.test.ts, and holds `"/v1"` as the chi mount base it prefixes Go
		// routes with) entered the population and reported a phantom `GET /v1` — a 404 on a route
		// nothing requests. A file that calls neither `apiRequest(` nor `fetch(` cannot issue a
		// request at all, so it cannot issue one to an unregistered path.
		//
		// ⚠⚠ NARROWING A POPULATION IS THE DANGEROUS DIRECTION AND THE FIRST DRAFT OF THIS VERY
		// EXCLUSION PROVED IT, ON ITSELF. It tested `strings.Contains(src, "apiRequest(")` — and
		// every call in this client is written `apiRequest<Type>(…`, so the substring never
		// appears and the predicate dropped TWENTY-FIVE files, i.e. the whole API client. It did
		// NOT surface as a phantom 404 in direction (a); it surfaced three files later as three
		// routes "with no SPA caller" in direction (b), which is the quieter half. The matcher is
		// a regex over `apiRequest` followed by `<` or `(` for exactly that reason.
		//
		// MEASURED at d1bfd11 with the correct matcher, by raising minSPAFiles to 999 and reading
		// the count out of the failure both ways: WITHOUT the exclusion the walk sees 92 verb+path
		// pairs across 21 files; WITH it, 91 across 20. It drops exactly ONE file and exactly ONE
		// pair — the phantom `GET /v1` — and 91 is the same pair count recorded at 97ba255.
		if !reIssuesRequest.MatchString(src) {
			return nil
		}

		helpers := map[string]string{}
		for _, m := range reAPIHelper.FindAllStringSubmatch(src, -1) {
			helpers[m[1]] = m[2]
		}
		// Remove the definitions first so substituting a NAME cannot rewrite its own body.
		src = reAPIHelper.ReplaceAllString(src, `const $1 = () => ""`)
		for name, body := range helpers {
			src = regexp.MustCompile(`\$\{`+regexp.QuoteMeta(name)+`\([^)]*\)\}`).ReplaceAllLiteralString(src, body)
			// ⚠ ReplaceAllString EXPANDS `$` IN THE REPLACEMENT. The helper body is full of
			// `${spaceID}`, so a template-based replacement silently substituted the empty
			// string for every parameter and produced `/v1/spaces//pages//edit-session` — three
			// phantom 404s in direction (a) AND three phantom unreached routes in (b), from one
			// mistake. ReplaceAllStringFunc does no expansion.
			re := regexp.MustCompile(`(^|[^\w$.])` + regexp.QuoteMeta(name) + `\([^)]*\)`)
			b, nm := body, name
			src = re.ReplaceAllStringFunc(src, func(m string) string {
				prefix := ""
				if !strings.HasPrefix(m, nm+"(") {
					prefix = m[:1]
				}
				return prefix + "`" + b + "`"
			})
		}

		var hits [][]int
		for _, m := range reAPILit.FindAllStringIndex(src, -1) {
			if strings.Contains(src[m[0]:m[1]], "/v1") {
				hits = append(hits, m)
			}
		}
		for k, m := range hits {
			lit := strings.Trim(src[m[0]:m[1]], "`\"'")
			idx := strings.Index(lit, "/v1")
			stop := len(src)
			if k+1 < len(hits) {
				stop = hits[k+1][0]
			}
			if m[1]+400 < stop {
				stop = m[1] + 400
			}
			verb := "GET"
			if mm := reAPIMethod.FindStringSubmatch(src[m[1]:stop]); mm != nil {
				verb = strings.ToUpper(mm[1])
			}
			// A WebSocket upgrade is served by a chi Get; the transport differs, the route does not.
			if strings.Contains(src[m[1]:min(m[1]+140, len(src))], "WebSocket") {
				verb = "GET"
			}
			out = append(out, spaReq{Verb: verb, Path: normAPIPath(lit[idx:]), File: rel})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk frontend/src: %v", err)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ⚠ FLOORS. Both sides report an ABSENCE, and an absence is what a broken scanner reports too.
// Measured at 97ba255: 91 distinct SPA verb+path pairs across 22 files, 100 routes under /v1.
const (
	minSPAPairs  = 82
	minSPAFiles  = 18
	minV1Routes  = 90
	minTableSize = 7
)

// ── (a) every SPA request reaches a registered route ────────────────────────────────────────────

func TestEverySPARequestReachesARegisteredRoute(t *testing.T) {
	reqs := spaRequests(t)
	pairs, byPath := serverRoutes(t)

	seenPairs, seenFiles := map[string]bool{}, map[string]bool{}
	for _, r := range reqs {
		seenPairs[r.Verb+" "+r.Path] = true
		seenFiles[r.File] = true
	}
	if len(seenPairs) < minSPAPairs || len(seenFiles) < minSPAFiles || len(pairs) < minV1Routes {
		t.Fatalf("a scanner went blind: %d SPA verb+path pairs across %d files, %d /v1 routes; "+
			"want >=%d, >=%d, >=%d. At 97ba255: 91, 22, 100. Do not lower these to turn a red "+
			"green — find out why the scan stopped seeing what it saw.",
			len(seenPairs), len(seenFiles), len(pairs), minSPAPairs, minSPAFiles, minV1Routes)
	}

	var missing, mismatched []string
	done := map[string]bool{}
	for _, r := range reqs {
		key := r.Verb + " " + r.Path
		if pairs[key] || done[key] {
			done[key] = true
			continue
		}
		done[key] = true
		if verbs, ok := byPath[r.Path]; ok {
			sort.Strings(verbs)
			mismatched = append(mismatched, fmt.Sprintf("%s (%s) — server serves %s there",
				key, r.File, strings.Join(verbs, ",")))
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (%s) — no route with that path at all", key, r.File))
	}
	sort.Strings(missing)
	sort.Strings(mismatched)
	if len(missing) > 0 {
		t.Errorf("%d SPA request(s) hit a path the router does not serve — a runtime 404 that "+
			"typechecks cleanly:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	if len(mismatched) > 0 {
		t.Errorf("%d SPA request(s) use a method the router does not serve on that path — a "+
			"runtime 405:\n  %s", len(mismatched), strings.Join(mismatched, "\n  "))
	}
}

// ── (b) the routes no SPA call reaches, and what does ───────────────────────────────────────────
//
// ⚠ THE RIGHT-HAND COLUMN IS THE POINT. "9 routes have no SPA caller" is a number; "four of them
// are reached by nothing, anywhere" is the finding, and only the tags separate them.

var routesWithNoSPACaller = map[string]string{
	// internal/block — see the header. 361 lines, a table, four permission-gated routes, and no
	// client in the estate. The `blocks` table can only ever be empty in production.
	"GET /v1/pages/{}/blocks":    "NO CALLER ANYWHERE IN THE ESTATE — internal/block",
	"POST /v1/pages/{}/blocks":   "NO CALLER ANYWHERE IN THE ESTATE — internal/block",
	"PATCH /v1/blocks/{}":        "NO CALLER ANYWHERE IN THE ESTATE — internal/block",
	"DELETE /v1/blocks/{}":       "NO CALLER ANYWHERE IN THE ESTATE — internal/block",
	"POST /v1/import/confluence": "documented in this repo's README; no client calls it",
	"POST /v1/import/notion":     "documented in this repo's README; no client calls it",
	"POST /v1/service/workspaces/{}/member-sync": "service-to-service — talyvor-suite's bff calls it " +
		"(apps/bff/docs_membersync_test.go, deploy/FULL-STACK-DEPLOY.md)",
	"GET /v1/workspaces/{}/changelog/feed": "named in talyvor-suite/BFF-GAPS.md as a gap; not yet called",
	// ⚠ A VERB-LEVEL ENTRY, AND THE REASON THE ESTATE TAGS SAY "PATH-LEVEL". frontend/src/api/
	// changelog.ts PATCHes and DELETEs this exact path and never GETs it. A path-level search
	// finds changelog.ts and would have called this route reached.
	"GET /v1/spaces/{}/pages/{}/changelog/entries/{}": "the SPA PATCHes and DELETEs this path " +
		"(api/changelog.ts) but never GETs it",
}

func TestRoutesWithNoSPACallerAreTheKnownSet(t *testing.T) {
	reqs := spaRequests(t)
	pairs, _ := serverRoutes(t)

	called := map[string]bool{}
	for _, r := range reqs {
		called[r.Verb+" "+r.Path] = true
	}
	got := map[string]bool{}
	for pair := range pairs {
		if !called[pair] {
			got[pair] = true
		}
	}

	var appeared, disappeared []string
	for pair := range got {
		if _, ok := routesWithNoSPACaller[pair]; !ok {
			appeared = append(appeared, pair)
		}
	}
	for pair := range routesWithNoSPACaller {
		if !got[pair] {
			disappeared = append(disappeared, pair)
		}
	}
	sort.Strings(appeared)
	sort.Strings(disappeared)

	if len(appeared) > 0 {
		t.Errorf("%d route(s) are registered with NO SPA caller and are not in the table:\n  %s\n\n"+
			"Add each with what DOES reach it — a sibling repo, a webhook, the README, or "+
			"\"NO CALLER ANYWHERE IN THE ESTATE\" if you looked and found none. An entry with no "+
			"reason is worse than no entry.", len(appeared), strings.Join(appeared, "\n  "))
	}
	if len(disappeared) > 0 {
		t.Errorf("%d route(s) in the table NOW HAVE an SPA caller:\n  %s\n\n"+
			"Almost certainly good news — a UI was wired to something that had none. Delete those "+
			"rows. This is the direction the table exists to notice.",
			len(disappeared), strings.Join(disappeared, "\n  "))
	}

	// ⚠ Without this, gutting the table makes both loops above vacuous and the test passes on an
	// empty census.
	if len(routesWithNoSPACaller) < minTableSize {
		t.Fatalf("the table has %d entries; 9 were measured at 97ba255. Below %d means routes were "+
			"wired up en masse or the table was gutted — say which.",
			len(routesWithNoSPACaller), minTableSize)
	}
	nowhere := 0
	for _, tag := range routesWithNoSPACaller {
		if strings.HasPrefix(tag, "NO CALLER ANYWHERE") {
			nowhere++
		}
	}
	if nowhere == 0 {
		t.Error("no row is tagged NO CALLER ANYWHERE IN THE ESTATE. Four were at 97ba255 — the " +
			"whole internal/block surface. If every one has since found a caller that is a real " +
			"and welcome change, but say so in the header rather than leaving an empty category.")
	}
}
