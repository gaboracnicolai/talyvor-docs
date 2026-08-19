// This file is the ORDER half of the suppression-premise class guard. Its sibling in this
// package, gate_premise_test.go, checks the FOURTEEN suppressions that state a CALL-GRAPH premise
// ("X is reached only via Y, which calls Z"). This one checks the other large family: the
// suppressions that state a claim about POSITION inside one function.
//
// THE POPULATION AND WHY IT WAS UNREAD. 44 suppressions are in the tree. TWENTY-ONE of them are
// justified by some form of "authorized ... before any store op" — and until this file, nothing
// read a single one of them. The sibling's census predicate is "reached only via", so all
// twenty-one fall outside it by construction; the sibling's own control pins that exclusion with
// a row spelling out one of these reasons verbatim. NONE of gofmt, `go vet`, the real-Postgres
// suite, the rule fixtures, check-semgrep-rule-scope.py or `semgrep --config .semgrep/ --error`
// reads a suppression's reason at all — so, exactly as with the two false `assertInWorkspaces`
// reasons the sibling found, these could go false with no commit near them and no diff to review.
//
// ⚠⚠ THIS GUARD PASSED ON ITS FIRST RUN, AND THAT IS A PROPERTY OF THE TREE, NOT EVIDENCE OF
// ANYTHING. All twenty-one premises were re-measured true at e1a335f before a line of this file
// was written — every enclosing function does call its named authorizer, and does call it before
// the value is used. A guard written against a population it has already confirmed correct cannot
// distinguish itself from a guard that asserts nothing, and this repo has shipped three of those.
// The controls in scripts/w31-authzorder-controls-7f3b.py are therefore the entire evidence that
// this file is a guard: the load-bearing one (A1) MOVES an authorize call BELOW the store op it
// protects, changing nothing about its presence, and requires this file to red on the ORDER.
//
// ⚠⚠ AND A1 IS CAUGHT HERE AND BY NOTHING ELSE IN THE REPO — MEASURED OVER THE FULL SUITE ON REAL
// POSTGRES, NOT ASSUMED. With internal/space#List reading the store BEFORE authorizing the path
// workspace, all 42 packages stay green, INCLUDING internal/space's own
// TestSEC4_WorkspaceRoutes_* cross-tenant test written for that very route. The reason is worth
// stating because it is why no HTTP test could ever close this: the gate still refuses, so the
// RESPONSE is byte-identical: a non-member still gets 403 and still gets no rows. What changed is
// that a query ran against a workspace the caller does not belong to — and "before any store op"
// is precisely what these twenty-one suppressions claim, so it is the one thing an assertion
// about the response cannot check. A3 (the refusal's `return` deleted from internal/page#Stale)
// is the same story with a second reason: h.visibleTo filters the rows afterwards, so the SEC-4
// test's own leak assertion correctly stays silent while an unauthorized read has still happened.
//
// ⚠ TWO PREDICTIONS IN THAT HARNESS WERE WRONG AND BOTH CORRECTIONS ARE RECORDED THERE RATHER
// THAN QUIETLY FIXED. A2 (delete internal/ai#Write's gate outright) was predicted to red the
// real-PG authz tests too and did not — all five AI routes and search are mounted through
// ratelimit.WorkspaceLimit("wsID"), which authorizes the same param before the handler runs, so
// the handler's own gate is defence in depth and deleting it changes no behaviour. A6a exposed
// something about THIS FILE: a reason cannot leave the census by being reworded while it still
// names its authorizer, because the predicate /(?i)authoriz/ matches the identifier
// AuthorizeWorkspace itself. That is a stronger property than the one the control set out to
// test, and it was only visible because the prediction was written down before the run.
//
// WHAT IT ASSERTS, and it is four things:
//
//	CENSUS   the set of suppressions stating an authorization premise is EXACTLY the pinned table
//	         below. A new one cannot join silently and a reworded one cannot drop out silently.
//	GATE     the authorizer the row names is called inside the suppression's own function.
//	ORDER    that call is positioned BEFORE every use of the suppressed value. This is the half
//	         the reasons actually claim ("before any store op") and the half no other instrument
//	         in this repo checks.
//	REFUSAL  the gate's verdict is acted on: the statement holding the call, or the one straight
//	         after it, is an `if` whose body RETURNS. A gate that is called and whose answer is
//	         dropped satisfies GATE and ORDER while authorizing nothing, which is the shape a
//	         presence-only check would bless.
//
// ⚠ ORDER IS SOURCE POSITION, NOT DOMINANCE, AND THE DIFFERENCE IS A REAL RESIDUE. A gate that
// appears first but runs conditionally — say wrapped in `if r.URL.Query().Get("skipauthz") == ""`
// — is positionally first and this file passes it. Control A7 performs exactly that mutation and is
// recorded as NOT CAUGHT rather than left unmeasured. Checking dominance needs a control-flow
// graph; for the shape these twenty-one actually have (read the param, authorize it, refuse, then
// use it — all at the top level of the function) position and execution order coincide, and that
// coincidence is asserted by the CENSUS half: a row that grows a branch has to be re-pinned here.
//
// ⚠ WHY THE PARAM IS FOUND FROM THE AST AND NOT FROM THE COMMENT'S TEXT. The reason strings are
// prose and vary ("on the next line", "here:", "below by"); parsing them would make the guard's
// population depend on wording, which is the failure mode the CENSUS rule exists to prevent. The
// suppressed line is located by NUMBER and the assignment it sits on is read from the syntax tree,
// so a rewording moves nothing except the row's own reason text.
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

var (
	// authzClaimRe selects this file's population: a reason arguing from AUTHORIZATION.
	authzClaimRe = regexp.MustCompile(`(?i)authoriz`)
	// siblingRe is gate_premise_test.go's population. A suppression belongs to exactly one of
	// the two censuses; without this, four of internal/comment's and block's call-graph rows
	// (whose prose also says "authorizes") would be claimed by both files and this one would
	// try to read an ordering claim out of a reachability one.
	siblingRe = regexp.MustCompile(`reached (?:from the handler )?only via`)
)

// kindOrder — the suppressed value IS used later, and the claim is that the gate runs first.
// kindGateOnly — the suppressed value is consumed by the gate and by NOTHING else; the reason
// says so ("the limiter keys on the returned Membership, never on this raw param"). For these,
// "the gate is first" is too weak: the assertion is that no later use exists at all, so one
// appearing is a red. It is the stronger claim, and it is the one that row actually makes.
// kindUpstream — the reason argues about the function's CALLERS, not about position inside it.
// Out of scope here and PINNED rather than filtered, so this file's green cannot be read as
// covering them; a filter that silently drops non-matches is how a real premise leaves a census.
type okind int

const (
	kindOrder okind = iota
	kindGateOnly
	kindUpstream
)

type oexpected struct {
	kind okind
	gate string // the authorizer identifier called in this function; "" for kindUpstream
	// n is how many suppressions in THIS function state an authorization premise. Without it a
	// function that already holds one could grow a second silently: the new row would look up
	// the same key, find it pinned, and be waved through by a census whose whole job is that
	// nothing joins unseen. internal/approval/store.go#Decide holds two suppressions today and
	// is the reason this is not hypothetical — only one of the two argues from authorization,
	// and which one that is has to be a pinned fact rather than an accident of wording.
	n    int
	note string
}

// ORDER_PREMISES is the census, keyed "<file>#<enclosing func>". Every row was re-measured at
// e1a335f, through the syntax tree rather than by reading the comment beside it.
var ORDER_PREMISES = map[string]oexpected{
	// ── the uniform shape: read {wsID}, authorize it, refuse, then use it. Nineteen of these.
	"internal/ai/handler.go#Write":                  {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/ai/handler.go#Transform":              {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/ai/handler.go#Translate":              {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/ai/handler.go#Ask":                    {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/ai/handler.go#SuggestTitle":           {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/analytics/handler.go#WorkspaceStats":  {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/approval/handler.go#Pending":          {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/changelog/handler.go#Feed":            {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/freshness/handler.go#Workspace":       {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/page/handler.go#Search":               {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/page/handler.go#Stale":                {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/search/handler.go#Search":             {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/space/handler.go#List":                {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/trackintegration/handler.go#GetIssue": {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/trackintegration/handler.go#Search":   {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/templatelib/handler.go#scopeFor":      {kindOrder, "AuthorizeWorkspace", 1, ""},
	"internal/templatelib/handler.go#FromPage":      {kindOrder, "AuthorizeWorkspace", 1, "the write TARGET, not a read scope: the {wsID} that will OWN the new template. authz.WorkspaceIDs is read one line earlier for the SOURCE page's scope, so pinning the gate by name is what keeps the order rule pointed at the right call"},
	"internal/customdomain/handler.go#scopeFor":     {kindOrder, "WorkspaceIDs", 1, "the only row whose gate is not AuthorizeWorkspace: it tests membership with contains(authz.WorkspaceIDs(ctx), wsID). Its templatelib twin does the same job with the other primitive, which is why the gate is pinned per row and not assumed"},

	// ── the param is consumed by the gate and by nothing else, and the reason says so.
	"internal/ratelimit/middleware.go#WorkspaceLimit": {kindGateOnly, "AuthorizeWorkspace", 1, "the limiter keys on the returned Membership; the raw param must reach nothing. This is also the tree's ONE indirect read — chi.URLParam(r, param) — so the literal semgrep rule cannot see the line at all and docs-no-indirect-url-param-scope is what its suppression names"},

	// ── claims about CALLERS, not about position inside this function. Out of scope for the
	//    ORDER rule and pinned rather than filtered, so this file's green cannot be read as
	//    covering them. The sibling's reachability caveat is the same shape.
	"internal/page/store.go#Update":     {kindUpstream, "", 1, "\"every caller authorizes the page upstream\" — a claim about the call graph of a primitive, checkable only with cross-package resolution. gate_premise_test.go's table already carries the UpdateInWorkspaces half of it"},
	"internal/approval/store.go#Decide": {kindUpstream, "", 1, "\"pageID is the page the ROUTE'S ENFORCER authorized\" — the gate is in a middleware in another package. NOTE Decide holds TWO by-id-write suppressions; only this one argues from authorization, which is what n=1 pins"},
}

// ─── the walk ───────────────────────────────────────────

type orow struct {
	file   string
	line   int
	reason string
}

func authzCensus(t *testing.T, root string) (rows []orow, goFiles, suppressionLines int) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
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
			if !authzClaimRe.MatchString(reason) || siblingRe.MatchString(reason) {
				continue
			}
			rows = append(rows, orow{file: rel, line: i + 1, reason: strings.TrimSpace(reason)})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if goFiles == 0 || suppressionLines < premiseFloor {
		t.Fatalf(`this census read %d non-test .go files and %d %s lines (floor %d).

A count near zero means the READER is broken — a moved directory, a changed suffix, a skip that
grew — and every assertion in this file would then pass over a population that is not there.`,
			goFiles, suppressionLines, marker, premiseFloor)
	}
	return rows, goFiles, suppressionLines
}

// ─── syntax ─────────────────────────────────────────────

// parsedFile holds one file's AST plus the fset needed to turn positions into line numbers.
type parsedFile struct {
	fset *token.FileSet
	file *ast.File
}

func parseOne(t *testing.T, path string) parsedFile {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return parsedFile{fset: fset, file: f}
}

func (p parsedFile) line(pos token.Pos) int { return p.fset.Position(pos).Line }

// enclosing returns the func declaration whose body spans the given line.
func (p parsedFile) enclosing(ln int) *ast.FuncDecl {
	for _, d := range p.file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		if p.line(fd.Pos()) <= ln && ln <= p.line(fd.End()) {
			return fd
		}
	}
	return nil
}

// funcName renders a decl as it is keyed in the census: the plain name.
func funcName(fd *ast.FuncDecl) string {
	if fd == nil || fd.Name == nil {
		return "?"
	}
	return fd.Name.Name
}

// calledName reports the identifier a call expression names, ignoring the receiver.
func calledName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if f.Sel != nil {
			return f.Sel.Name
		}
	}
	return ""
}

// paramAssign finds the assignment that reads the suppressed path param. The suppression sits on
// the assignment's own line, or on the line immediately above it when the comment needs a line of
// its own (internal/ratelimit is that shape). Returns the assignment and the name it binds.
func (p parsedFile) paramAssign(fd *ast.FuncDecl, ln int) (*ast.AssignStmt, string) {
	var found *ast.AssignStmt
	var name string
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || found != nil {
			return true
		}
		l := p.line(as.Pos())
		if l != ln && l != ln+1 {
			return true
		}
		reads := false
		for _, rhs := range as.Rhs {
			ast.Inspect(rhs, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok && calledName(c) == "URLParam" {
					reads = true
				}
				return true
			})
		}
		if !reads || len(as.Lhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		found, name = as, id.Name
		return false
	})
	return found, name
}

// gateCall returns the FIRST call to the named authorizer in fd, by source position.
func firstCallTo(fd *ast.FuncDecl, gate string) *ast.CallExpr {
	var first *ast.CallExpr
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok || calledName(c) != gate {
			return true
		}
		if first == nil || c.Pos() < first.Pos() {
			first = c
		}
		return true
	})
	return first
}

// usesOf lists every occurrence of the bound name in fd, EXCLUDING the assignment that binds it
// and the gate call that consumes it — the two places the premise is about rather than at risk
// from. What is left is precisely "the places this raw, attacker-controlled value is used".
func usesOf(fd *ast.FuncDecl, name string, skipAssign *ast.AssignStmt, skipGate *ast.CallExpr) []token.Pos {
	var out []token.Pos
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		if skipAssign != nil && id.Pos() >= skipAssign.Pos() && id.End() <= skipAssign.End() {
			return true
		}
		if skipGate != nil && id.Pos() >= skipGate.Pos() && id.End() <= skipGate.End() {
			return true
		}
		out = append(out, id.Pos())
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// refusalActedOn reports whether the gate's verdict causes a return. Two shapes exist in the
// tree and both are accepted: the call inside an `if` init or condition whose body returns, and
// the call in a plain assignment whose very next statement is such an `if`.
func refusalActedOn(fd *ast.FuncDecl, gate *ast.CallExpr) bool {
	if gate == nil {
		return false
	}
	acted := false
	var walk func(stmts []ast.Stmt)
	returns := func(s ast.Stmt) bool {
		found := false
		ast.Inspect(s, func(n ast.Node) bool {
			if _, ok := n.(*ast.ReturnStmt); ok {
				found = true
			}
			return !found
		})
		return found
	}
	contains := func(n ast.Node) bool {
		return n != nil && n.Pos() <= gate.Pos() && gate.End() <= n.End()
	}
	walk = func(stmts []ast.Stmt) {
		for i, s := range stmts {
			switch st := s.(type) {
			case *ast.IfStmt:
				// the call lives in this if's own init or condition
				if contains(st.Init) || contains(st.Cond) {
					if returns(st.Body) {
						acted = true
					}
					return
				}
				walk(st.Body.List)
				if st.Else != nil {
					if b, ok := st.Else.(*ast.BlockStmt); ok {
						walk(b.List)
					} else {
						walk([]ast.Stmt{st.Else})
					}
				}
			case *ast.AssignStmt:
				if contains(st) && i+1 < len(stmts) {
					if next, ok := stmts[i+1].(*ast.IfStmt); ok && returns(next.Body) {
						acted = true
					}
					return
				}
			case *ast.BlockStmt:
				walk(st.List)
			case *ast.ExprStmt:
				// a func literal body (middleware) holds the real statements
				ast.Inspect(st, func(n ast.Node) bool {
					if fl, ok := n.(*ast.FuncLit); ok && fl.Body != nil {
						walk(fl.Body.List)
					}
					return true
				})
			case *ast.ReturnStmt:
				ast.Inspect(st, func(n ast.Node) bool {
					if fl, ok := n.(*ast.FuncLit); ok && fl.Body != nil {
						walk(fl.Body.List)
					}
					return true
				})
			}
			if acted {
				return
			}
		}
	}
	walk(fd.Body.List)
	return acted
}

// ─── the assertions ─────────────────────────────────────

func TestAuthzOrderPremises(t *testing.T) {
	root := repoRoot(t)
	rows, goFiles, lines := authzCensus(t, root)
	t.Logf("read %d non-test .go files, %d %s lines, %d stating an authorization premise",
		goFiles, lines, marker, len(rows))

	seen := map[string]bool{}
	count := map[string]int{}
	var unpinned []string

	for _, r := range rows {
		p := parseOne(t, filepath.Join(root, r.file))
		fd := p.enclosing(r.line)
		if fd == nil {
			t.Errorf("%s:%d — no enclosing func for a suppression stating an authorization premise:\n  %s",
				r.file, r.line, r.reason)
			continue
		}
		key := r.file + "#" + funcName(fd)
		count[key]++
		want, pinned := ORDER_PREMISES[key]
		if !pinned {
			unpinned = append(unpinned, fmt.Sprintf("\t%q: {kindOrder, %q, 1, %q},\t// %s:%d",
				key, "AuthorizeWorkspace", "", r.file, r.line))
			continue
		}
		seen[key] = true
		if want.kind == kindUpstream {
			continue
		}

		assign, name := p.paramAssign(fd, r.line)
		if assign == nil {
			t.Errorf("%s#%s (line %d) — pinned as an in-function ordering premise, but no path-param "+
				"assignment was found on that line. Either the suppression moved or the row is "+
				"misclassified; a row whose subject cannot be located asserts nothing.\n  %s",
				r.file, funcName(fd), r.line, r.reason)
			continue
		}
		gate := firstCallTo(fd, want.gate)
		if gate == nil {
			t.Errorf("GATE — %s#%s says it is authorized by %s and %s does not call it.\n  %s",
				r.file, funcName(fd), want.gate, funcName(fd), r.reason)
			continue
		}
		if !refusalActedOn(fd, gate) {
			t.Errorf("REFUSAL — %s#%s calls %s and does not act on its verdict: the statement holding "+
				"the call, and the one after it, do not return. A gate whose answer is dropped "+
				"authorizes nothing while still being present and first.\n  %s",
				r.file, funcName(fd), want.gate, r.reason)
		}
		uses := usesOf(fd, name, assign, gate)
		switch want.kind {
		case kindGateOnly:
			if len(uses) != 0 {
				t.Errorf("GATE-ONLY — %s#%s claims the raw param %q reaches nothing but %s, and it is "+
					"used at line(s) %v. The claim is not that the gate is first; it is that no other "+
					"use exists.\n  %s",
					r.file, funcName(fd), name, want.gate, linesOf(p, uses), r.reason)
			}
		case kindOrder:
			if len(uses) == 0 {
				t.Errorf("ORDER — %s#%s reads %q and never uses it again. An ordering premise over a "+
					"value with no later use is a claim about nothing; if the read is genuinely "+
					"gate-only, pin it kindGateOnly so that a use appearing later is a red.\n  %s",
					r.file, funcName(fd), name, r.reason)
				continue
			}
			if first := uses[0]; first < gate.Pos() {
				t.Errorf("ORDER — %s#%s uses the unauthorized path param %q at line %d, and %s is not "+
					"called until line %d. The suppression's whole argument is that the authorization "+
					"happens FIRST.\n  %s",
					r.file, funcName(fd), name, p.line(first), want.gate, p.line(gate.Pos()), r.reason)
			}
		}
	}

	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		t.Errorf(`%d suppression(s) state an authorization premise and are not in ORDER_PREMISES.

A premise that is not pinned is not checked. Add each row below (correcting its kind and gate),
which is the point: the claim then gets verified instead of merely written down.

%s`, len(unpinned), strings.Join(unpinned, "\n"))
	}

	var miscounted []string
	for k, want := range ORDER_PREMISES {
		if got := count[k]; seen[k] && got != want.n {
			miscounted = append(miscounted, fmt.Sprintf("%s: pinned %d, found %d", k, want.n, got))
		}
	}
	if len(miscounted) > 0 {
		sort.Strings(miscounted)
		t.Errorf(`%d function(s) hold a different number of authorization premises than pinned:
	%s

The key is the FUNCTION, so a second suppression added to a function that already has one would
otherwise find the key pinned and be waved through — a silent join into the one census that
exists to make joining loud.`, len(miscounted), strings.Join(miscounted, "\n\t"))
	}

	var missing []string
	for k := range ORDER_PREMISES {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf(`%d pinned premise(s) were not found in the tree: %s

A row leaves this census when its suppression is deleted (fine — delete the row) or when its
reason is REWORDED past the census predicate (not fine — the claim is still being made and is no
longer being read). Deciding which is the review this file exists to force.`,
			len(missing), strings.Join(missing, ", "))
	}
}

func linesOf(p parsedFile, pos []token.Pos) []int {
	out := make([]int, 0, len(pos))
	for _, x := range pos {
		out = append(out, p.line(x))
	}
	return out
}
