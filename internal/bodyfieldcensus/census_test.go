// Package bodyfieldcensus holds one repository-wide census: no request-body struct may DECLARE
// an identity / tenancy / privilege field that its handler never touches.
//
// ⚠ WHY THIS EXISTS, AND WHY THE TWO SEMGREP RULES CANNOT SEE IT.
// .semgrep/body-supplied-authority.yml is the guard for this class. Rule A fires on a decoded
// struct REACHING A STORE without an approved resolver; rule B fires on an identity field READ
// off the decoded body and never assigned. Both are anchored on a USE — a read or a store call.
// A field that is decoded and then mentioned NOWHERE is invisible to both by construction, and
// rule B's own message asks for exactly the thing neither rule enforces: "delete the field from
// the request struct: an endpoint that accepts no authority cannot be lied to."
//
// ⚠ MEASURED AT b98933b, NOT ARGUED — the live instance this census was built on.
// internal/comment/handler.go's replyBody declared an AuthorID string with the json tag
// author_id, while Reply passed actorFor(r) and read in.AuthorID nowhere. Its SIBLING createBody carries a comment
// saying "the field is gone" — for createBody it was; for replyBody nobody removed it. The whole
// gauntlet was green with it there: gofmt clean, go vet exit 0, semgrep --config .semgrep/
// --error EXIT 0 with zero findings, and the full Go suite on real Postgres green. It is the
// exact shape internal/comment and internal/pagelock each SHIPPED as live defects (a
// two-workspace member posted comments authored as anyone; the same member unlocked another
// member's page lock by naming them) — one `actorFor(r)` -> `in.AuthorID` edit away from being
// live again, and advertised to every client that reads the API surface.
//
// ⚠ THE POPULATION IS RECOMPUTED FROM THE DECODE SITES ON EVERY RUN, WHICH IS THE WHOLE POINT.
// A guard that iterated a hand-written list of "body structs" would be satisfied by construction
// and would watch the next one go in. This parses every non-test .go file, finds every
// `json.NewDecoder(<*http.Request>.Body).Decode(&x)`, resolves x's declared type — including
// anonymous struct literals and cross-package named types, via the file's own import table —
// and reads the fields off THAT declaration.
//
// ⚠ AND THE SAME GUARD WOULD PASS ON ITS FIRST RUN ONCE THE ONE LIVE FIELD IS DELETED, BY
// CONSTRUCTION. That is the trap #177 recorded for the three MCP tool sets. It was run RED
// FIRST against b98933b with replyBody.AuthorID still present (it named it and failed), and the
// positive controls in scripts/w31-bodyfield-controls-7k2v.py arm a fake identity field on a
// DIFFERENT body struct in a DIFFERENT package, blind the population, and blind the exempt list.
//
// WHAT THIS IS NOT:
//   - It is NOT a dataflow analysis. "Mentioned" means the field appears as <target>.<Field>
//     anywhere in the enclosing handler — a READ or an ASSIGNMENT both count, because a handler
//     that assigns the field has taken control of it and rule B already owns the read case.
//   - It CANNOT see a handler that passes the whole decoded value to a HELPER which reads the
//     field there. Such a field reads as unmentioned here; the answer is to delete it or to
//     exempt it with the reason, and either way the decision is a visible line in a diff.
//   - It CANNOT see a `map[string]any` body at all: a map declares no fields, so any key the
//     caller invents arrives. There are FOUR such targets and they are counted and named by
//     TestBodyDecodeTargetsAllResolve so the limit is visible rather than implied. They are
//     covered instead by their handlers' own key allowlists (see internal/page/store.go's note
//     on c3daaf7 shutting ai_cost_usd on Update's allowlist).
//   - It does NOT judge whether an exempted field is SAFE. It closes the gap between "someone
//     decided" and "nobody noticed".
package bodyfieldcensus

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/talyvor/docs"

// identityJSONField is the wire-name shape that makes a field an AUTHORITY claim. It is the
// json tag, not the Go name, because the wire name is what a caller actually sends. The list is
// the union of every field this class has produced a real defect on (workspace_id, created_by,
// viewer_id, author_id, is_admin) plus the identity-shaped names already censused across the
// tree at 11c234b when handover item (c) was scoped.
var identityJSONField = regexp.MustCompile(`^(workspace_id|author_id|viewer_id|member_id|user_id|actor_id|created_by|updated_by|verified_by|verifier_id|owner_id|requester_id|requested_by|decided_by|locked_by|reviewer_id|resolved_by|granted_by|editor_id|client_id|is_admin|is_owner)$`)

// decodedButUnusedExempt names every identity-shaped field that a request body decodes and its
// handler never mentions, WITH THE REASON IT STAYS. A new one must be deleted from the request
// struct or named here — either way a reviewer sees the decision.
//
// ⚠ ALL THREE ENTRIES ARE THE SAME SITE AND THE SAME CAUSE: internal/page/handler.go Create
// decodes the WHOLE model.Page ROW TYPE from the body, so it inherits every column's field
// whether or not the create path has anything to do with it. The fields cannot be deleted —
// model.Page is the row type the store scans into — and they are inert at the write:
// MEASURED at b98933b, `INSERT INTO pages` exists in exactly ONE place in this repository
// (internal/page/store.go) and its column list is
//
//	(space_id, workspace_id, parent_id, title, slug, content, content_text, icon, cover_url,
//	 position, depth, is_template, created_by, updated_by, linked_issues, stale_after_days,
//	 page_type)
//
// with updated_by bound to `p.CreatedBy` — the value Handler.Create assigns from
// permission.ActorFromContext — and verified_by / locked_by absent from the statement entirely.
// The response is `RETURNING`, so it reports the row, not the body.
//
// ⚠ THAT THIS SITE DECODES A ROW TYPE AT ALL IS A SEPARATE FINDING AND IS RECORDED IN THE QUEUE,
// NOT FIXED HERE: giving Create a body-only struct is a different merge. These three lines are
// what stops that decision from being silent in the meantime.
var decodedButUnusedExempt = map[string]string{
	"internal/model.Page.UpdatedBy":  "model.Page is the pages ROW type, not a body-only struct; the single INSERT binds updated_by to p.CreatedBy (the verified actor), never to p.UpdatedBy",
	"internal/model.Page.VerifiedBy": "model.Page is the pages ROW type, not a body-only struct; verified_by is not a column of the single INSERT",
	"internal/model.Page.LockedBy":   "model.Page is the pages ROW type, not a body-only struct; locked_by is not a column of the single INSERT",
}

// nonStructBodyTargets names every request-body decode target that is NOT a struct, so the
// census cannot read fields off it. Each must be named here with what covers it instead — an
// unresolved target that nobody listed would silently shrink the population, which is how a
// census goes blind while staying green.
var nonStructBodyTargets = map[string]string{
	"internal/changelog/handler.go:Update:map[string]any":    "PATCH body; the handler applies a fixed key allowlist",
	"internal/database/handler.go:UpdateView:map[string]any": "PATCH body; the handler applies a fixed key allowlist",
	"internal/page/handler.go:Update:map[string]any":         "PATCH body; the handler applies a fixed key allowlist (c3daaf7 shut ai_cost_usd on it)",
	"internal/space/handler.go:Update:map[string]any":        "PATCH body; the handler applies a fixed key allowlist",
}

type bodyField struct {
	key      string // "<pkg dir>.<type>.<Field>" — stable across line moves
	file     string // repo-relative, slash-separated
	line     int
	handler  string
	jsonName string
}

type decodeSite struct {
	file    string
	line    int
	handler string
	target  string
	typeStr string
	st      *ast.StructType
	// mentions counts <target>.<Field> occurrences anywhere in the enclosing handler.
	mentions map[string]int
	typeDir  string
}

type censusResult struct {
	sites       []decodeSite
	unmentioned []bodyField
	goFiles     int
	packages    int
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/bodyfieldcensus -> repo root
}

// census parses the tree and returns every request-body decode site plus every identity-shaped
// field on a decoded struct that the enclosing handler never mentions.
func census(t *testing.T, root string) censusResult {
	t.Helper()
	fset := token.NewFileSet()

	type parsed struct {
		f    *ast.File
		dir  string // repo-relative package dir
		rel  string // repo-relative file path
		imps map[string]string
	}
	var files []parsed
	structs := map[string]map[string]*ast.StructType{} // dir -> type name -> struct
	dirs := map[string]bool{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			// .git / node_modules: not Go source. .semgrep: the rule FIXTURES are deliberately
			// violating Go whose whole purpose is to carry this defect — censusing them would
			// measure the test data, not the product. vendor: not ours.
			case ".git", "node_modules", ".semgrep", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		dirs[dir] = true

		imps := map[string]string{}
		for _, im := range f.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			name := path_base(p)
			if im.Name != nil {
				name = im.Name.Name
			}
			imps[name] = p
		}
		files = append(files, parsed{f: f, dir: dir, rel: rel, imps: imps})

		if structs[dir] == nil {
			structs[dir] = map[string]*ast.StructType{}
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				ts, ok := s.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[dir][ts.Name.Name] = st
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	res := censusResult{goFiles: len(files), packages: len(dirs)}

	for _, p := range files {
		for _, d := range p.f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			reqVars := map[string]bool{}
			if fd.Type.Params != nil {
				for _, pa := range fd.Type.Params.List {
					if !isHTTPRequestPtr(pa.Type) {
						continue
					}
					for _, n := range pa.Names {
						reqVars[n.Name] = true
					}
				}
			}
			if len(reqVars) == 0 {
				continue
			}
			localType := declaredTypes(fd.Body)

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				target, ok := bodyDecodeTarget(n, reqVars)
				if !ok {
					return true
				}
				st, typeStr, typeDir := resolveType(localType[target], p.dir, p.imps, structs)
				mentions := map[string]int{}
				ast.Inspect(fd.Body, func(m ast.Node) bool {
					se, ok := m.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if id, ok := se.X.(*ast.Ident); ok && id.Name == target {
						mentions[se.Sel.Name]++
					}
					return true
				})
				res.sites = append(res.sites, decodeSite{
					file: p.rel, line: fset.Position(n.Pos()).Line, handler: fd.Name.Name,
					target: target, typeStr: typeStr, st: st, mentions: mentions, typeDir: typeDir,
				})
				return true
			})
		}
	}

	sort.Slice(res.sites, func(i, j int) bool {
		if res.sites[i].file != res.sites[j].file {
			return res.sites[i].file < res.sites[j].file
		}
		return res.sites[i].line < res.sites[j].line
	})

	for _, s := range res.sites {
		if s.st == nil {
			continue
		}
		for _, fl := range s.st.Fields.List {
			jsonName := ""
			if fl.Tag != nil {
				v, uerr := strconv.Unquote(fl.Tag.Value)
				if uerr == nil {
					jsonName = strings.Split(reflect.StructTag(v).Get("json"), ",")[0]
				}
			}
			for _, nm := range fl.Names {
				wire := jsonName
				if wire == "" {
					// No tag: encoding/json matches the field name case-insensitively, so the
					// lowercased Go name is what a caller can send.
					wire = strings.ToLower(nm.Name)
				}
				if wire == "-" || !identityJSONField.MatchString(wire) {
					continue
				}
				if s.mentions[nm.Name] > 0 {
					continue
				}
				res.unmentioned = append(res.unmentioned, bodyField{
					key:      fieldKey(s, nm.Name),
					file:     s.file,
					line:     s.line,
					handler:  s.handler,
					jsonName: wire,
				})
			}
		}
	}
	sort.Slice(res.unmentioned, func(i, j int) bool { return res.unmentioned[i].key < res.unmentioned[j].key })
	return res
}

// TestNoDecodedButUnusedIdentityField is the census. Every identity-shaped field a request body
// decodes and its handler never mentions must be deleted or exempted with a reason.
func TestNoDecodedButUnusedIdentityField(t *testing.T) {
	res := census(t, repoRoot(t))
	for _, f := range res.unmentioned {
		if _, ok := decodedButUnusedExempt[f.key]; ok {
			continue
		}
		t.Errorf(`DECODED-BUT-UNUSED AUTHORITY FIELD: %s (%s:%d, handler %s) declares json:"%s" and %s mentions it NOWHERE.
The endpoint accepts an authority claim it does not use — invisible to .semgrep rules A and B,
which both anchor on a use — and one edit away from being the defect internal/comment and
internal/pagelock each shipped. Delete the field from the request struct (rule B's own message
asks for this), or add %q to decodedButUnusedExempt with the reason it must stay.`,
			f.key, f.file, f.line, f.handler, f.jsonName, f.handler, f.key)
	}
	if t.Failed() {
		t.Logf("census read %d Go files across %d packages, %d request-body decode sites",
			res.goFiles, res.packages, len(res.sites))
	}
}

// TestExemptListHasNoStaleEntries stops the exempt list from rotting into a list of names that
// no longer exist — a stale exemption is an unfalsifiable claim, and it would also silently
// pre-approve a field that came back under the same name later.
func TestExemptListHasNoStaleEntries(t *testing.T) {
	res := census(t, repoRoot(t))
	live := map[string]bool{}
	for _, f := range res.unmentioned {
		live[f.key] = true
	}
	for key := range decodedButUnusedExempt {
		if !live[key] {
			t.Errorf("STALE EXEMPTION: decodedButUnusedExempt names %q, which is no longer a "+
				"decoded-but-unmentioned identity field. Delete the line.", key)
		}
	}
}

// TestBodyDecodeTargetsAllResolve is the instrument check. A decode target whose type this
// census cannot resolve reads a struct with no fields — silently zero findings. Every target
// must resolve to a struct, or be named in nonStructBodyTargets with what covers it instead.
func TestBodyDecodeTargetsAllResolve(t *testing.T) {
	res := census(t, repoRoot(t))

	// Floors, not equalities: they prove the walk read the tree rather than an empty directory,
	// and they do not have to be edited every time a handler is added.
	//
	// ⚠ THE FIRST VERSION OF THIS FLOOR WAS 100 AND THE TREE HAS 81 NON-TEST GO FILES, so it
	// could only ever fail — the mirror image of a guard that can only pass, and caught the same
	// way, by running it. The numbers below sit UNDER the measured values (81 files, 33 sites) by
	// enough that adding or removing a handler does not touch them.
	if res.goFiles < 60 {
		t.Fatalf("census read only %d non-test Go files — the walk is not reaching the tree", res.goFiles)
	}
	if len(res.sites) < 25 {
		t.Fatalf("census found only %d request-body decode sites — the matcher has stopped matching", len(res.sites))
	}

	seen := map[string]bool{}
	for _, s := range res.sites {
		if s.st != nil {
			continue
		}
		key := fmt.Sprintf("%s:%s:%s", s.file, s.handler, s.typeStr)
		seen[key] = true
		if _, ok := nonStructBodyTargets[key]; !ok {
			t.Errorf(`UNRESOLVED BODY TARGET: %s:%d %s decodes into %q, which this census cannot read
fields off. It is therefore counted as ZERO identity fields. Either give the handler a named
struct, or add %q to nonStructBodyTargets with what covers it instead.`,
				s.file, s.line, s.handler, s.typeStr, key)
		}
	}
	for key := range nonStructBodyTargets {
		if !seen[key] {
			t.Errorf("STALE nonStructBodyTargets ENTRY: %q no longer matches any unresolved "+
				"decode target. Delete the line.", key)
		}
	}
}

// fieldKey names one field so an exemption survives a line move. A NAMED type is keyed by the
// package that DECLARES it plus the bare type name, so the same shared row type reached from two
// handlers is one key and not two. An ANONYMOUS struct has no name to key on, so it is keyed by
// the file and handler that declares it — there is exactly one body decode per handler in this
// repository, and if that ever stops being true the key collides visibly rather than silently.
func fieldKey(s decodeSite, field string) string {
	if s.st != nil && s.typeStr == "anonymous struct" {
		return s.file + ":" + s.handler + ".<anon>." + field
	}
	name := s.typeStr
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return s.typeDir + "." + name + "." + field
}

// ─── AST helpers ──────────────────────────────────────────────────────────

// bodyDecodeTarget matches `json.NewDecoder(<req>.Body).Decode(&x)` and returns "x". It requires
// <req> to be a parameter of type *http.Request so that decoding an outbound RESPONSE body
// (trackintegration, lenscreds, lensintegration all do) is not counted: those are not
// client-supplied request bodies and are a different question.
func bodyDecodeTarget(n ast.Node, reqVars map[string]bool) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Decode" || len(call.Args) != 1 {
		return "", false
	}
	inner, ok := sel.X.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	isel, ok := inner.Fun.(*ast.SelectorExpr)
	if !ok || isel.Sel.Name != "NewDecoder" || len(inner.Args) != 1 {
		return "", false
	}
	if pkg, ok := isel.X.(*ast.Ident); !ok || pkg.Name != "json" {
		return "", false
	}
	bsel, ok := inner.Args[0].(*ast.SelectorExpr)
	if !ok || bsel.Sel.Name != "Body" {
		return "", false
	}
	bid, ok := bsel.X.(*ast.Ident)
	if !ok || !reqVars[bid.Name] {
		return "", false
	}
	arg := call.Args[0]
	if u, ok := arg.(*ast.UnaryExpr); ok && u.Op == token.AND {
		arg = u.X
	}
	id, ok := arg.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// declaredTypes maps local variable name -> its declared type expression, for `var x T` and
// `x := T{...}`. Those are the two spellings every decode site in this repository uses.
func declaredTypes(body *ast.BlockStmt) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.DeclStmt:
			gd, ok := v.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				for _, nm := range vs.Names {
					out[nm.Name] = vs.Type
				}
			}
		case *ast.AssignStmt:
			if v.Tok != token.DEFINE || len(v.Lhs) != len(v.Rhs) {
				return true
			}
			for i, l := range v.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok {
					continue
				}
				if cl, ok := v.Rhs[i].(*ast.CompositeLit); ok && cl.Type != nil {
					out[id.Name] = cl.Type
				}
			}
		}
		return true
	})
	return out
}

// resolveType returns the struct declaration behind a decode target's type, its printed form,
// and the package dir the type is DECLARED in. Cross-package names are resolved through the
// file's own import table rather than by package base name, so two packages sharing a base name
// cannot be confused for each other.
func resolveType(te ast.Expr, dir string, imps map[string]string, structs map[string]map[string]*ast.StructType) (*ast.StructType, string, string) {
	switch v := te.(type) {
	case nil:
		return nil, "<undeclared>", dir
	case *ast.StructType:
		// An anonymous struct literal: the declaration is right here.
		return v, "anonymous struct", dir
	case *ast.Ident:
		return structs[dir][v.Name], v.Name, dir
	case *ast.SelectorExpr:
		pkgIdent, ok := v.X.(*ast.Ident)
		if !ok {
			return nil, exprString(te), dir
		}
		imp, ok := imps[pkgIdent.Name]
		if !ok || !strings.HasPrefix(imp, modulePath+"/") {
			return nil, exprString(te), dir
		}
		d := strings.TrimPrefix(imp, modulePath+"/")
		return structs[d][v.Sel.Name], exprString(te), d
	default:
		return nil, exprString(te), dir
	}
}

func isHTTPRequestPtr(e ast.Expr) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := st.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http" && sel.Sel.Name == "Request"
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case nil:
		return ""
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(v.X)
	case *ast.ArrayType:
		return "[]" + exprString(v.Elt)
	case *ast.MapType:
		return "map[" + exprString(v.Key) + "]" + exprString(v.Value)
	case *ast.InterfaceType:
		if v.Methods == nil || len(v.Methods.List) == 0 {
			return "any"
		}
		return "interface{...}"
	case *ast.StructType:
		return "anonymous struct"
	default:
		return fmt.Sprintf("%T", e)
	}
}

func path_base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
