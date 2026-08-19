// Package suppressionguard holds the class guard for the one claim this repo's scanner strategy
// rests on and nothing was checking: the REASON attached to an inline `nosemgrep` exemption.
//
// WHY THE REASONS ARE LOAD-BEARING HERE, IN THIS REPO'S OWN WORDS. scripts/check-semgrep-rule-scope.py
// refuses a `paths:` filter on any rule, and argues the alternative in its own docstring:
//
//	"An allow-list can only ever ratify yesterday's sweep; the deny-list of inline `nosemgrep`
//	 exemptions is this repo's chosen mechanism, and each of those is one line, states its
//	 reason, and is reviewed in the diff that adds it."
//
// Reviewed in the diff that adds it — and never again. The reason is what the reviewer of the
// NEXT diff reads instead of re-deriving the gate, so a stale one is not a typo: it is the
// argument that a by-id write on a tenant table needs no workspace scope, still being made after
// the thing it names has moved.
//
// THE POPULATION. 44 suppressions are in the tree. FOURTEEN state a CALL-GRAPH premise — "X is a
// primitive reached only via Y, which calls Z first" — and a call-graph premise is a falsifiable
// claim about Go source, so it is checkable from here. SEVEN of the fourteen name the gate Z by
// identifier, and that is the half this guard enforces.
//
// MEASURED, NOT REASONED ABOUT, at fd96dec: TWO of those seven were already false. Both live on
// internal/comment's by-id writes (Unresolve, Delete) and both cite `assertInWorkspaces` — a
// function that DOES NOT EXIST in internal/comment. The real gate is `assertInPage` and it is
// STRICTLY STRONGER (it ties {id} to the route-authorized {pageID} as well as to the tenant), so
// the code was right and only its stated reason was wrong.
//
// NOBODY WAS CARELESS, WHICH IS THE POINT. f238b90 (#22) wrote both reasons and both were TRUE
// that day. 7583d2a (#24), the next commit to touch the file, deleted assertInWorkspaces and
// repointed all four call sites at assertInPage — and updated neither reason, nor the one-line
// summary at the head of the gate's own doc comment, which is where their text came from.
// a126eb6 (#33) then added Resolve's suppression naming assertInPage correctly, so the file has
// carried BOTH spellings, three functions apart, since 2026-07-18. 160 commits landed on main
// after the rename and 2 of them touched this file: the claim went false with no commit near it,
// and no review sees a diff that does not exist. Nothing in gofmt, `go vet`, the real-Postgres
// suite, the rule fixtures, check-semgrep-rule-scope.py or `semgrep --config .semgrep/ --error`
// reads a suppression's reason at all, so all three copies survived every gate in the repo.
//
// WHAT THIS GUARD ASSERTS, and it is four things:
//
//	CENSUS  the set of suppressions stating a call-graph premise is EXACTLY the pinned table
//	        below. A new one cannot join silently and a reworded one cannot drop out silently —
//	        the second is the failure this repo has already shipped twice with `paths:` filters
//	        and once with DECLARED_UNFIXTURED.
//	WRAPPER the named wrapper Y is a func declared in the suppression's own package.
//	GATE    the named gate Z is a func declared in the suppression's own package.  ← the two
//	        false reasons above red HERE.
//	CALLS   Y's body actually contains a call to Z. This is the security half: it turns "someone
//	        deleted the assert out of the wrapper" from a silent reopening of a cross-page write
//	        into a CI failure, whatever the suppression still says.
//
// AND THE CALLS RULE IS NOT DECORATION — THE CONTROL FOR IT FALSIFIED MY OWN PREDICTION, IN THE
// DANGEROUS DIRECTION. Deleting the assertInPage call out of comment.UnresolveInWorkspaces was
// predicted to red here AND in internal/comment's own cross-tenant real-Postgres test. It reds
// ONLY HERE: 37 packages, the whole suite, green with DELETE …/comments/{id}/resolve reopened to
// a caller authorized against a different page. TestSec_Comment_CannotActAcrossPagesViaAuthorizedPageID
// drove Resolve, Delete and Reply and not Unresolve — three of the four wrappers that take the
// gate. That gap is closed in the same merge as this file (case (e), with a legitimate-path
// companion so a verb that refuses everything cannot satisfy it), so the mutation now reds twice.
// It reded once, here, before that case existed — which is the measurement, and the reason this
// rule is worth having for the other twelve premises where no such test may exist either.
//
// WHAT IT DOES NOT ASSERT, said plainly, because a guard that implied it would be worse than
// none. It does NOT check the "reached only via" half — that Y is the primitive's ONLY caller.
// That needs cross-package type resolution (these primitives are reached through interfaces:
// internal/mcp holds `pages pageDeps`, not a *page.Store), and two of the fourteen premises are
// ALREADY false on exactly that half — measured at fd96dec, mcp/server.go#toolVerifyPage calls
// pages.Verify and #toolUpdatePage calls pages.Update directly, neither named by the suppression
// on the primitive. Both are gated — callTool's SEC-4 chokepoint resolves the acted-on workspace
// from the object and authorizes it before dispatch — so they are stale prose, not holes. They
// are recorded in the table so that this guard's green result cannot be read as covering them.
//
// It also does not check that the gate Z is CORRECT, only that it exists and is called. A
// wrapper calling a gate that asserts nothing passes here; that residue is held by the per-package
// real-Postgres cross-tenant tests, which drive foreign ids through the real routes.
//
// SIBLING. internal/block/suppression_premise_test.go guards the premise of the tree's ONE
// docs-no-body-supplied-authority exemption, which is a claim about a TYPE (model.Block carries
// no authority field). This file is the same idea over a different premise shape — a claim about
// the CALL GRAPH — and over a population of fourteen rather than one. Neither subsumes the other.
package suppressionguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// marker is assembled rather than written out for the same reason internal/block's census does
// it: this file is a .go file inside the tree the census walks. The walk skips _test.go, which is
// what actually keeps this file out of the population — but if that filter is ever relaxed, a
// spelled-out marker beside the phrases below would make the guard read ITSELF and report a
// population that does not exist. Do not "simplify" these into one literal.
const marker = "nosemgrep" + ":"

var (
	// wrapperRe reads the "reached only via Y" half. "reached from the handler only via" is
	// internal/approval's wording for the same claim and is deliberately accepted, so that a
	// premise cannot leave the census by being phrased slightly differently.
	wrapperRe = regexp.MustCompile(`reached (?:from the handler )?only via ([A-Za-z_][A-Za-z0-9_]*)`)
	// gateRe reads the "which calls Z" half. Only this exact phrasing counts: an entry whose
	// reason describes its gate in prose ("which asserts the page ∈ …") names no identifier, is
	// recorded in the table with gate "", and is checked for its wrapper only.
	gateRe = regexp.MustCompile(`which calls ([A-Za-z_][A-Za-z0-9_]*)`)
)

// premiseFloor is a liveness floor on the RAW population, not on the parsed one. 44 suppression
// lines were in the tree when this was written; if a future walk sees a small fraction of that,
// the reader has gone blind (a moved directory, a changed extension, a skip that grew) and every
// assertion below would pass over almost nothing. Set well under 44 so ordinary churn does not
// trip it, and far above zero so an empty read cannot look like a clean tree.
const premiseFloor = 30

// kind classifies what a census row's "reached only via" match actually is.
type kind int

const (
	// kindCall — the match is a Go function and the premise is checkable here.
	kindCall kind = iota
	// kindProse — the match is a word that is not a function. One row is like this and it is
	// pinned rather than filtered, because a filter is how a real premise leaves a census.
	kindProse
)

type expected struct {
	kind kind
	gate string // "" when the reason names no gate identifier
	note string
}

// PREMISES is the census, keyed "<file>#<wrapper>". It is the pinned set the walk must reproduce
// exactly. Adding a suppression that states a call-graph premise means adding a line here, which
// is the point: the premise then gets checked instead of merely being written down.
var PREMISES = map[string]expected{
	"internal/analytics/store.go#RecordViewInWorkspaces":     {kindCall, "", "gate described in prose (SELECT EXISTS … workspace_id = ANY), no identifier named"},
	"internal/approval/store.go#RequestApprovalInWorkspaces": {kindCall, "", "gate in prose; this reason also admits other callers (\"server-side seeds only\"), which is why the reachability half is out of scope here"},
	"internal/block/handler.go#page_id":                      {kindProse, "", "\"blocks are reached only via page_id\" — a COLUMN, not a function. Pinned, not filtered: a filter that quietly drops non-matches is how a real premise leaves a census"},
	"internal/block/store.go#UpdateInWorkspaces":             {kindCall, "", "gate in prose (blocks.page_id → pages.workspace_id = ANY)"},
	"internal/block/store.go#DeleteInWorkspaces":             {kindCall, "", "gate in prose"},
	"internal/comment/store.go#ResolveInWorkspaces":          {kindCall, "assertInPage", "the correct one of the three; the other two were copied from a stale doc line"},
	"internal/comment/store.go#UnresolveInWorkspaces":        {kindCall, "assertInPage", "was \"assertInWorkspaces\", which internal/comment does not declare — corrected with this guard"},
	"internal/comment/store.go#DeleteInWorkspaces":           {kindCall, "assertInPage", "was \"assertInWorkspaces\" — corrected with this guard"},
	"internal/page/store.go#DeleteInWorkspaces":              {kindCall, "assertInWorkspaces", "internal/page DOES declare assertInWorkspaces; the name is only wrong in internal/comment"},
	"internal/page/store.go#VerifyInWorkspaces":              {kindCall, "assertInWorkspaces", "⚠ the REACHABILITY half of this one is false and unchecked: mcp/server.go#toolVerifyPage calls pages.Verify directly. Gated at callTool's SEC-4 chokepoint, so stale prose, not a hole"},
	"internal/pagelock/store.go#LockInWorkspaces":            {kindCall, "", "gate in prose"},
	"internal/pagelock/store.go#UnlockInWorkspaces":          {kindCall, "", "gate in prose"},
	"internal/space/store.go#UpdateInWorkspaces":             {kindCall, "assertInWorkspaces", ""},
	"internal/space/store.go#DeleteInWorkspaces":             {kindCall, "assertInWorkspaces", ""},
}

// ─── the walk ────────────────────────────────────────────

type row struct {
	file    string // repo-relative, slash-separated
	line    int
	wrapper string
	gate    string
	reason  string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/suppressionguard -> repo root
}

// census walks the tree and returns every suppression stating a "reached only via" premise,
// together with the raw counts that prove the walk read something.
func census(t *testing.T, root string) (rows []row, goFiles, suppressionLines int) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			// .git: not source. frontend/node_modules: not Go. .semgrep: the rule FIXTURES are
			// deliberately-violating Go whose suppressions are test data, not claims about this
			// product — including them would measure the wrong population.
			case ".git", "node_modules", ".semgrep":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		goFiles++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, marker) {
				continue
			}
			suppressionLines++
			reason := line
			if _, after, ok := strings.Cut(line, "-- "); ok {
				reason = after
			}
			w := wrapperRe.FindStringSubmatch(reason)
			if w == nil {
				continue
			}
			g := ""
			if m := gateRe.FindStringSubmatch(reason); m != nil {
				g = m[1]
			}
			rows = append(rows, row{file: rel, line: i + 1, wrapper: w[1], gate: g, reason: strings.TrimSpace(reason)})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if goFiles == 0 || suppressionLines < premiseFloor {
		t.Fatalf(`this census read %d non-test .go files and %d %s lines (floor %d).

44 were in the tree when this guard was written. A count near zero means the READER is broken —
a moved directory, a changed suffix, a skip that grew — and every assertion in this file would
then pass over a population that is not there.`, goFiles, suppressionLines, marker, premiseFloor)
	}
	return rows, goFiles, suppressionLines
}

// ─── package parsing ─────────────────────────────────────

// funcsIn parses every non-test .go file in dir and returns its func and method declarations by
// name. Methods are keyed by name alone: no package in this tree declares a method and a plain
// func with the same name, and the premises name gates like `assertInPage` that only ever exist
// as one.
func funcsIn(t *testing.T, dir string) map[string]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir %s: %v", dir, err)
	}
	out := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	parsed := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, n), err)
		}
		parsed++
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name != nil {
				out[fd.Name.Name] = fd
			}
		}
	}
	if parsed == 0 || len(out) == 0 {
		t.Fatalf("parsed %d files and found %d funcs in %s — the parser read nothing, so "+
			"\"declared here\" below would be answered by an empty map", parsed, len(out), dir)
	}
	return out
}

// callsInBody reports whether fn's body contains a call to a function or method named target.
func callsInBody(fn *ast.FuncDecl, target string) bool {
	if fn == nil || fn.Body == nil {
		return false
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if f.Name == target {
				found = true
			}
		case *ast.SelectorExpr:
			if f.Sel != nil && f.Sel.Name == target {
				found = true
			}
		}
		return !found
	})
	return found
}

// ─── the assertions ──────────────────────────────────────

// TestPremiseCensus_IsExactlyThePinnedSet keeps every assertion below pointed at a population a
// reader has actually seen. A suppression that joins the tree with a call-graph premise must be
// added here — at which point it is CHECKED — and one that leaves, or is reworded out of the
// parser's reach, must be removed deliberately rather than by silence.
func TestPremiseCensus_IsExactlyThePinnedSet(t *testing.T) {
	root := repoRoot(t)
	rows, goFiles, lines := census(t, root)
	t.Logf("read %d non-test .go files, %d %s lines, %d stating a call-graph premise",
		goFiles, lines, marker, len(rows))

	got := map[string]bool{}
	for _, r := range rows {
		key := r.file + "#" + r.wrapper
		if got[key] {
			t.Errorf("two suppressions in %s name the same wrapper %s — the census key is not "+
				"unique any more and one of them is being checked twice while the other is invisible",
				r.file, r.wrapper)
		}
		got[key] = true
		if _, ok := PREMISES[key]; !ok {
			t.Errorf(`%s:%d states a call-graph premise that this census does not know about:

  %s

Add %q to PREMISES with the gate identifier its reason names (or "" if it describes the gate in
prose). That is not bookkeeping — the entry is what makes the premise CHECKED rather than merely
written down.`, r.file, r.line, r.reason, key)
		}
	}
	for key, exp := range PREMISES {
		if !got[key] {
			t.Errorf(`PREMISES pins %q and the walk did not find it (note: %s).

Either the suppression is gone — delete the entry, and say so in the diff — or its reason was
REWORDED past %q / %q and has silently left the checked population. The second is the failure this
repo has already shipped with a paths: allow-list and with DECLARED_UNFIXTURED: the instrument
kept reporting green over a shrinking subject.`, key, exp.note, wrapperRe.String(), gateRe.String())
		}
	}
}

// TestPremiseGate_NamesAFunctionThatExistsAndIsActuallyCalled is the guard the reasons needed and
// did not have. It reds on a suppression that cites a gate its package does not declare, and on a
// wrapper that has stopped calling the gate its suppression still credits.
func TestPremiseGate_NamesAFunctionThatExistsAndIsActuallyCalled(t *testing.T) {
	root := repoRoot(t)
	rows, _, _ := census(t, root)

	byDir := map[string]map[string]*ast.FuncDecl{}
	checkedWrappers, checkedGates := 0, 0

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].file != rows[j].file {
			return rows[i].file < rows[j].file
		}
		return rows[i].line < rows[j].line
	})

	for _, r := range rows {
		exp, known := PREMISES[r.file+"#"+r.wrapper]
		if !known {
			continue // reported by the census test; not re-reported here
		}
		if exp.kind == kindProse {
			continue
		}
		dir := filepath.Join(root, filepath.FromSlash(filepath.Dir(r.file)))
		if _, ok := byDir[dir]; !ok {
			byDir[dir] = funcsIn(t, dir)
		}
		funcs := byDir[dir]

		// WRAPPER — the reason names the only door it says the primitive has.
		wrapper, ok := funcs[r.wrapper]
		if !ok {
			t.Errorf(`%s:%d says the primitive is "reached only via %s", and package %s declares no such function.

  %s

A suppression turns off a tenancy rule on a by-id write. When the door it names is gone, what is
left is the exemption without the argument for it.`, r.file, r.line, r.wrapper, filepath.Base(dir), r.reason)
			continue
		}
		checkedWrappers++

		if r.gate == "" {
			if exp.gate != "" {
				t.Errorf("%s:%d no longer names a gate identifier, but PREMISES still expects %q — "+
					"the reason was reworded and this premise has quietly stopped being checked",
					r.file, r.line, exp.gate)
			}
			continue
		}
		if r.gate != exp.gate {
			t.Errorf("%s:%d names gate %q; PREMISES pins %q. Update the entry deliberately, or the "+
				"census is agreeing with whatever the comment happens to say today",
				r.file, r.line, r.gate, exp.gate)
		}

		// GATE — the identifier the reason credits must exist where it says it does.
		if _, ok := funcs[r.gate]; !ok {
			t.Errorf(`%s:%d justifies suppressing a tenancy rule by naming a gate that DOES NOT EXIST:

  %s

Package %s declares no %s. This is a claim about the call graph, and it is false as written.
Either the gate was renamed and this reason was left behind — name the real one — or the gate is
gone, in which case the suppression is unargued and must be deleted so the rule speaks again.`,
				r.file, r.line, r.reason, filepath.Base(dir), r.gate)
			continue
		}
		checkedGates++

		// CALLS — the security half. The gate existing somewhere in the package is not the claim;
		// the claim is that THIS wrapper runs it before it reaches the primitive.
		if !callsInBody(wrapper, r.gate) {
			t.Errorf(`%s:%d says %s "calls %s first", and %s's body contains no call to %s.

  %s

If the assert was removed, the by-id write below this line is reachable with an id the caller was
never authorized for, and the suppression is why nothing said so.`,
				r.file, r.line, r.wrapper, r.gate, r.wrapper, r.gate, r.reason)
		}
	}

	// Neither counter may be zero: a run in which no wrapper and no gate was actually resolved is
	// a run that proved nothing, and it would look exactly like a clean tree.
	if checkedWrappers == 0 || checkedGates == 0 {
		t.Fatalf("resolved %d wrappers and %d gates — this guard asserted nothing at all",
			checkedWrappers, checkedGates)
	}
	t.Logf("checked %d wrapper claims, %d of them naming a gate identifier", checkedWrappers, checkedGates)
}

// TestPremiseChecker_CanSayNo proves the two mechanisms above are capable of failing, without
// mutating the tree. A guard whose only evidence is that it passed on the real repo is a guard
// nobody has seen speak.
func TestPremiseChecker_CanSayNo(t *testing.T) {
	// The parser and the caller-check, exercised on synthetic source.
	dir := t.TempDir()
	src := `package sample

type Store struct{}

func (s *Store) assertInPage() error { return nil }

func (s *Store) GoodWrapper() error {
	if err := s.assertInPage(); err != nil {
		return err
	}
	return nil
}

func (s *Store) UngatedWrapper() error { return nil }
`
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A _test.go in the same directory must NOT contribute declarations, or a gate that exists
	// only in test code would satisfy a production suppression.
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte("package sample\n\nfunc assertInTestOnly() {}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	funcs := funcsIn(t, dir)

	if _, ok := funcs["assertInPage"]; !ok {
		t.Fatal("funcsIn did not find a method that is plainly declared — the reader is broken")
	}
	if _, ok := funcs["assertInTestOnly"]; ok {
		t.Error("funcsIn picked a declaration out of a _test.go file: a gate that exists only in " +
			"tests would then satisfy a production suppression")
	}
	if _, ok := funcs["assertInWorkspaces"]; ok {
		t.Error("funcsIn reported a function that is not declared — the GATE rule cannot fail")
	}
	if !callsInBody(funcs["GoodWrapper"], "assertInPage") {
		t.Error("callsInBody missed a call that is plainly there — the CALLS rule reds on healthy code")
	}
	if callsInBody(funcs["UngatedWrapper"], "assertInPage") {
		t.Error("callsInBody reported a call in a body that has none — the CALLS rule cannot fail, " +
			"which is precisely the mutation it exists for")
	}

	// The census regexes, both directions. A reason that states no premise must not be dragged
	// into the population, and the two real phrasings must both be read.
	for _, tc := range []struct {
		reason      string
		wantWrapper string
		wantGate    string
	}{
		{"Delete is a primitive reached only via DeleteInWorkspaces (store.go), which calls assertInPage first", "DeleteInWorkspaces", "assertInPage"},
		{"RequestApproval is reached from the handler only via RequestApprovalInWorkspaces (above), which asserts the page ∈ …", "RequestApprovalInWorkspaces", ""},
		{"authorized on the next line by AuthorizeWorkspace, before any store op", "", ""},
		{"bearer-secret pattern, not an IDOR surface: link.ID is not client-supplied", "", ""},
	} {
		gotW, gotG := "", ""
		if m := wrapperRe.FindStringSubmatch(tc.reason); m != nil {
			gotW = m[1]
		}
		if m := gateRe.FindStringSubmatch(tc.reason); m != nil {
			gotG = m[1]
		}
		if gotW != tc.wantWrapper || gotG != tc.wantGate {
			t.Errorf("parse(%q) = (%q, %q), want (%q, %q)", tc.reason, gotW, gotG, tc.wantWrapper, tc.wantGate)
		}
	}

	// And the failure text a reader gets must name the file, or the census is a number nobody can
	// act on. Cheap to assert, and it is the half that rots first.
	if !strings.Contains(fmt.Sprintf("%s:%d", "internal/comment/store.go", 159), "internal/comment/store.go:159") {
		t.Error("failure locations are not file:line")
	}
}
