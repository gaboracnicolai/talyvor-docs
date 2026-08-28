package ai

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A FIXTURE THAT SENDS A KEY ITS HANDLER DOES NOT BIND IS A TEST OF SOMETHING ELSE.
//
// ── WHAT THIS IS FOR ─────────────────────────────────────────────────────────
//
// Every AI route here decodes an ANONYMOUS request struct — `var in struct{…}` followed by
// `json.NewDecoder(r.Body).Decode(&in)` — so the wire contract is a set of json tags and nothing
// else states it. A body naming a key outside that set does not fail: encoding/json ignores
// unknown fields, the field keeps its zero value, and the handler proceeds.
//
// ⚠ AND THE ZERO VALUE IS NOT ALWAYS INERT. `Translate` hands a blank `Language` to
// `Engine.Translate`, which substitutes `defaultLang = "English"` (engine.go) and interpolates it
// into the system prompt. So a fixture that misnames that key drives a REAL completion in English
// while reading, to anyone scanning the file, as a test that translated something into French.
//
// MEASURED at bd3d809, which is why this file exists: `handler_test.go` sent
// `{"text":"hello","target_language":"French"}` at `/ai/translate`, and the handler binds
// `json:"language"`. The case passes — it asserts only a 200, because it exists to prove the read
// gate is none of that route's business — so nothing anywhere said the language was silently wrong.
//
// ⚠ talyvor-suite HAD ALREADY BEEN GUARDED AGAINST THAT EXACT NAME FROM THREE DIRECTIONS
// (`areas/docs/api.ts` states the binding, `PageTranslation.tsx` records the measured three-way
// result, `pageTranslation.test.tsx` asserts the property is absent from the request body), and at
// bd3d809 the line below was the LAST occurrence of `target_language` in any repository. The
// product was never at risk; the upstream's own fixture was the only thing still sending it. This
// guard is the cheap question nobody had asked: does THIS repo's own test traffic use the keys THIS
// repo's own handlers bind?
//
// ── WHAT IT CHECKS, AND WHAT IT DELIBERATELY DOES NOT ────────────────────────
//
// It pairs a ROUTE PATH with the JSON body sent to it — the two appear adjacently, as
// `do(path, body)` arguments or as `{path, body}` in a table — and requires the body's top-level
// keys to be a subset of that route's bound tags. Both populations are DERIVED: the tags from
// handler.go's parse tree, the pairs from the test files' parse trees.
//
// ⚠ A BODY NOT ADJACENT TO ITS PATH IS NOT CHECKED, and that is a limit rather than an oversight.
// The alternative — treat every JSON literal in the package as a request body and test it against
// the UNION of all five key sets — reds on Lens response fixtures and on any unrelated JSON, and a
// guard that cries wolf is the one that gets deleted. The floors below are what keep the narrow
// rule from quietly becoming a rule about nothing.

// aiRoutes are the five mounted in Handler.Mount. Named here so a route added there without a
// fixture, or a route removed, moves a number this file asserts rather than passing unnoticed.
var aiRoutes = []string{"write", "transform", "translate", "ask", "suggest-title"}

// boundTags returns route suffix -> the json tags of the struct that route decodes from r.Body.
//
// The request struct is identified STRUCTURALLY, not by position: a `var <n> struct{…}` inside the
// handler func whose body also contains `Decode(&<n>)`. Keying on "the first struct in the func"
// would pick up a response struct the day one is declared above the decode.
func boundTags(t *testing.T) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "handler.go", nil, 0)
	if err != nil {
		t.Fatalf("parse handler.go: %v", err)
	}

	// handler func name -> route suffix, read from Mount so the mapping is not a second literal.
	mounted := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Post" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		path, err := strconv.Unquote(lit.Value)
		if err != nil || !strings.Contains(path, "/ai/") {
			return true
		}
		suffix := path[strings.LastIndex(path, "/")+1:]
		// h.limited(h.Write) — the handler is the inner selector.
		ast.Inspect(call.Args[1], func(m ast.Node) bool {
			if s, ok := m.(*ast.SelectorExpr); ok && s.Sel.IsExported() {
				mounted[s.Sel.Name] = suffix
			}
			return true
		})
		return true
	})

	out := map[string][]string{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		suffix, ok := mounted[fn.Name.Name]
		if !ok {
			continue
		}
		decoded := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Decode" {
				return true
			}
			if u, ok := call.Args[0].(*ast.UnaryExpr); ok && u.Op == token.AND {
				if id, ok := u.X.(*ast.Ident); ok {
					decoded[id.Name] = true
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || !decoded[vs.Names[0].Name] {
				return true
			}
			st, ok := vs.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, fld := range st.Fields.List {
				if fld.Tag == nil {
					continue
				}
				tag, err := strconv.Unquote(fld.Tag.Value)
				if err != nil {
					continue
				}
				if k := jsonName(tag); k != "" {
					out[suffix] = append(out[suffix], k)
				}
			}
			return true
		})
		sort.Strings(out[suffix])
	}
	return out
}

func jsonName(tag string) string {
	i := strings.Index(tag, `json:"`)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(`json:"`):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	name := rest[:j]
	if c := strings.Index(name, ","); c >= 0 {
		name = name[:c]
	}
	if name == "-" {
		return ""
	}
	return name
}

type fixture struct {
	file  string
	route string
	keys  []string
	body  string
}

// fixtures finds every (AI route path, JSON object) pair written adjacently in this package's tests.
func fixtures(t *testing.T) []fixture {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files globbed: %v", err)
	}
	var out []fixture
	for _, name := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var elts []ast.Expr
			switch v := n.(type) {
			case *ast.CallExpr:
				elts = v.Args
			case *ast.CompositeLit:
				elts = v.Elts
			default:
				return true
			}
			for i := 0; i+1 < len(elts); i++ {
				route := aiRouteOf(elts[i])
				if route == "" {
					continue
				}
				keys, body := jsonKeys(elts[i+1])
				if keys == nil {
					continue
				}
				out = append(out, fixture{file: name, route: route, keys: keys, body: body})
			}
			return true
		})
	}
	return out
}

func aiRouteOf(e ast.Expr) string {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil || !strings.Contains(s, "/ai/") {
		return ""
	}
	return s[strings.LastIndex(s, "/")+1:]
}

func jsonKeys(e ast.Expr) ([]string, string) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return nil, ""
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(s), "{") {
		return nil, ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &m) != nil {
		return nil, ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, s
}

func TestAIFixturesSendOnlyKeysTheHandlersBind(t *testing.T) {
	bound := boundTags(t)

	// ⚠ FLOOR ONE — the ROUTE population. With Mount unparsed this map is empty, every fixture
	// below is skipped as "no such route", and the loop passes having compared nothing.
	if len(bound) != len(aiRoutes) {
		t.Fatalf("parsed %d AI request structs out of handler.go, want %d (%v). Either Mount or "+
			"the `var in struct{…}` + Decode(&in) shape moved, and with it this whole check — a "+
			"census that parses no routes compares every fixture against nothing.",
			len(bound), len(aiRoutes), bound)
	}
	for _, r := range aiRoutes {
		if len(bound[r]) == 0 {
			t.Fatalf("/ai/%s bound no json tags. A route whose key set reads as empty accepts "+
				"every fixture key silently, which is the failure this file exists to catch.", r)
		}
	}

	found := fixtures(t)
	// ⚠ FLOOR TWO — the FIXTURE population. The pairing rule is narrow on purpose (a body must sit
	// next to its path), so it is exactly the kind of rule that can quietly stop matching. This
	// number moves when fixtures are added or removed; it must not move because the parse broke.
	const minFixtures = 5
	if len(found) < minFixtures {
		t.Fatalf("paired %d (route, body) fixtures in this package, floor is %d. The adjacency "+
			"rule stopped matching — every assertion below is drawn from that population.",
			len(found), minFixtures)
	}

	// ⚠ FLOOR TWO IS A COUNT, AND A COUNT IS THE WEAKER HALF OF THE QUESTION. MEASURED: the
	// adjacency rule pairs 15 fixtures against a floor of 5, so the parse can stop matching TEN of
	// them — two thirds of the population — and this file still reports a clean run. Worse, a
	// count cannot notice WHICH ones went: five fixtures all pointing at /ai/ask satisfy it while
	// four routes go entirely unchecked, and the fixture that started this file was a
	// /ai/translate one.
	//
	// So the floor that matters is PER ROUTE, and it is derived rather than chosen: every route
	// this package mounts must have at least one paired body. That is stable under adding or
	// removing individual fixtures — which the count deliberately allows — while a broken parse,
	// a renamed route, or a rewritten table drops a route to zero and says which.
	byRoute := map[string]int{}
	for _, fx := range found {
		byRoute[fx.route]++
	}
	for _, r := range aiRoutes {
		if byRoute[r] == 0 {
			t.Errorf("/ai/%s has NO paired (route, body) fixture in this package. Either nothing "+
				"exercises it, or — far more likely, since the count floor above passed — the "+
				"adjacency rule stopped matching the shape its fixtures are written in. Every key "+
				"assertion for this route is then drawn from an empty population.\n"+
				"    paired per route: %v", r, byRoute)
		}
	}

	for _, fx := range found {
		allowed := map[string]bool{}
		for _, k := range bound[fx.route] {
			allowed[k] = true
		}
		if len(allowed) == 0 {
			t.Errorf("%s sends a body to /ai/%s, which Mount does not mount. Either the fixture "+
				"names a route that does not exist, or a route was renamed and this fixture is "+
				"now testing a 404.", fx.file, fx.route)
			continue
		}
		for _, k := range fx.keys {
			if allowed[k] {
				continue
			}
			t.Errorf("%s sends %q to /ai/%s, which binds only %v.\n"+
				"  encoding/json IGNORES the unknown key, so the field keeps its ZERO VALUE and "+
				"the handler runs anyway — this fixture is exercising a request it does not look "+
				"like it is exercising, and it passes.\n"+
				"  body: %s",
				fx.file, k, fx.route, bound[fx.route], fx.body)
		}
	}
}
