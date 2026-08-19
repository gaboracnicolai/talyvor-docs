// Package paramcensus holds the guard that keeps .semgrep/operate-by-id-tenancy.yml's URL-param
// classification MEASURED rather than REMEMBERED.
//
// WHAT IT PROTECTS. Two rules in that file partition every `chi.URLParam` read in the tree:
//
//	docs-no-url-param-workspace-scope   owns exactly ONE literal — "wsID", the workspace.
//	docs-no-indirect-url-param-scope    owns everything it cannot place: a name arriving as a
//	                                    function argument, or a literal nobody has classified.
//
// Between them sits a CENSUS — a list, inside the second rule, of the literal param names measured
// not to be a workspace (pageID, dbID, spaceID, …). Without it, dropping the old blanket
// "exclude every string literal" would have made ~92 ordinary resource-id reads unclassified
// findings, and a class guard that demands 92 meaningless suppressions is a class guard that gets
// switched off.
//
// WHY THE LIST NEEDS A GUARD AT ALL — this is the whole point of the file. The rule set it replaced
// failed in exactly this shape TWICE. First a `paths.include` allow-list of ten packages, which
// omitted four that read {wsID}, one of them shipping the very defect the rule exists to catch.
// Then a blanket `pattern-not: chi.URLParam($R, "...")` justified in-file by "Every literal read is
// the sibling rule's business" — the sibling's business is one literal. Both times the remembered
// thing drifted from the measured thing and the comment beside it still read as an assurance.
// A census in a rule is the same species. It is only safe if something re-measures it.
//
// SO THIS ASSERTS, IN BOTH DIRECTIONS:
//
//	(1) every non-"wsID" literal the tree actually reads is IN the rule's census — otherwise the
//	    scan reddens on ordinary code and the next hand deletes the clause rather than reads it;
//	(2) every name in the rule's census is STILL READ by the tree — a dead exemption is a name
//	    that left the code but kept its pass, sitting there to be re-used for something else. That
//	    direction is the one no scan can see: semgrep is silent about a name nobody uses, and
//	    silence is what a shrinking allow-list looks like from the outside;
//	(3) the two rules agree about WHICH literal is the workspace. The sibling's `pattern` and the
//	    indirect rule's `pattern-not` both name "wsID" today. If one is edited and the other is
//	    not, the partition develops either a hole (nobody owns "wsID") or a double report, and
//	    (1) and (2) would both still pass while it happened.
//
// ⚠ THE GUARD THAT WAS PROPOSED NEXT ON THIS THREAD AND MEASURED NOT WORTH BUILDING — recorded
// here because this is where the next hand will look for it. The handover after #183 named a
// census over the ROUTE TREE: assert that the only path param a workspace-scoped mount introduces
// is "wsID", and catch a route that MOUNTS {pageID} while its handler READS "pageId" (chi.URLParam
// returns "" and a store op runs on an empty id). Both halves were probed and both were already
// held: the workspace-name half by #183 itself — an unclassified name is only dangerous once it is
// READ, and at that moment the semgrep rule fires — and the mismatch half by the suite, measured on
// a gated route (4 tests red across 3 packages, one of them routeguard's STRUCTURAL in-class route
// set) and on an ungated public one. scripts/w31-mountread-probe-8v3r.py is that measurement, kept
// runnable rather than written down: re-run it before building the census, because the ungated
// shape rests on ONE behavioural test that is not about this class at all.
//
// WHAT IT DOES NOT CLAIM, stated so the comment cannot be overread. It does not know what a name
// MEANS. Nothing here can tell that "tenantID" would be a workspace; someone could add it to the
// census and this stays green. What it guarantees is narrower and still worth having: the exempt
// set equals the read set, so an exemption is always a live, reviewed, written-down decision
// rather than a leftover. The MEANING is held at the point the census is edited — a rule diff with
// a reason, in the pull request that introduces the name.
//
// THE POPULATION BOUNDARY, AND WHICH HALF OF IT IS ACTUALLY DOING WORK — measured, because the
// two look the same in the source. The walk covers internal/ and cmd/, skipping _test.go.
//
//	· THE DIRECTORY CHOICE IS LOAD-BEARING. .semgrep/tests/ reads "workspaceID" and "tenantID" on
//	  purpose — they are the fixture's proof that an unclassified name is flagged, and they must
//	  never be in the census. Control C7 adds that directory to the walk and this guard reds,
//	  naming both.
//	· THE _test.go SKIP IS NOT, TODAY. Censused at d34e01c: the only param literal any _test.go
//	  passes to chi.URLParam through a real call is "pageID", already in the census (the "<name>"
//	  in routeguard is prose in a comment, which go/ast does not see). So removing that filter
//	  would change nothing right now. It is kept because the population this census is ABOUT is
//	  the code that serves requests — but it is not a claim, and saying it is load-bearing would
//	  be the kind of assurance-beside-a-gap this whole rule file exists to stop.
package paramcensus

import (
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

// The directories that hold production code. .semgrep/tests/ and _test.go files are excluded on
// purpose (see the package comment); frontend/ holds no Go.
var productionDirs = []string{"internal", "cmd"}

// ruleFile is parsed as TEXT, not as YAML, and that is deliberate: the thing under test is the
// exact bytes semgrep will load. A YAML round-trip would happily normalise away a duplicated key
// or a clause indented under the wrong parent — both of which change what the rule matches.
const ruleFile = ".semgrep/operate-by-id-tenancy.yml"

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/paramcensus -> repo root
}

func readRuleFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), ruleFile))
	if err != nil {
		t.Fatalf("read %s: %v", ruleFile, err)
	}
	return string(b)
}

// censusRe pulls the alternatives out of the indirect rule's metavariable-regex.
//
// ⚠ THE ANCHORING IS THE GUARD'S OWN VACUITY RISK AND IT IS HANDLED BY FAILING, NEVER BY
// RETURNING EMPTY. If the clause is deleted, renamed, or re-indented, this does not match — and
// censusNames() calls t.Fatalf rather than handing back an empty set. An empty set would make
// direction (2) vacuously true and direction (1) fail with a confusing message about 15 missing
// names, i.e. the guard would still be red but for the wrong reason and with the wrong fix.
// Measured: with the metavariable-regex clause commented out, this fails with "census clause not
// found", which names the actual edit.
var censusRe = regexp.MustCompile(`(?m)^\s*regex:\s*\^"\(([^)]*)\)"\$\s*$`)

// workspaceLiteralRe finds every `chi.URLParam($R, "<lit>")` that is an ACTUAL PATTERN CLAUSE in
// the rule file — the sibling's `pattern:` and the indirect rule's `pattern-not:`.
//
// ⚠ ANCHORED ON THE YAML KEY, AND THE FIRST VERSION WAS NOT. It matched the file's raw text, so it
// read the rule's own PROSE: the comment recording the old blanket clause quotes
// `pattern-not: chi.URLParam($R, "...")`, and the guard duly reported that this repo's workspace
// param had been renamed to "...". A guard that reads comments is measuring the wrong file.
var workspaceLiteralRe = regexp.MustCompile(`(?m)^\s*(?:-\s+)?pattern(?:-not)?:\s*chi\.URLParam\(\$R,\s*"([^"]+)"\)\s*$`)

func censusNames(t *testing.T) []string {
	t.Helper()
	m := censusRe.FindStringSubmatch(readRuleFile(t))
	if m == nil {
		t.Fatalf("census clause not found in %s: expected a line `regex: ^\"(a|b|c)\"$` under "+
			"docs-no-indirect-url-param-scope's metavariable-regex. If that clause was deliberately "+
			"removed, this guard must be removed or rewritten in the SAME commit — leaving it here "+
			"matching nothing is how a guard starts passing for free.", ruleFile)
	}
	names := strings.Split(m[1], "|")
	sort.Strings(names)
	return names
}

// treeLiterals walks the production directories and returns every literal param name passed to
// chi.URLParam, keyed to one example site so a failure names a file rather than a set difference.
//
// Parsed with go/ast rather than grepped: a grep counts the name in a comment, in a string, and in
// a _test.go file it was told to skip, and this guard's whole value is that its two sides are the
// same population.
func treeLiterals(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for _, dir := range productionDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			t.Fatalf("production dir %s missing: %v — this guard censuses a population that must exist", dir, err)
		}
		err := filepath.Walk(base, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				return perr
			}
			rel, _ := filepath.Rel(root, p)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 2 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "URLParam" {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "chi" {
					return true
				}
				lit, ok := call.Args[1].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true // a non-literal name: the indirect rule's other half, not this census
				}
				name, uerr := strconv.Unquote(lit.Value)
				if uerr != nil {
					return true
				}
				if _, seen := out[name]; !seen {
					out[name] = rel + ":" + strconv.Itoa(fset.Position(lit.Pos()).Line)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if len(out) == 0 {
		t.Fatalf("censused ZERO chi.URLParam literals across %v — the walk is broken, not the tree. "+
			"A guard that finds nothing agrees with any census.", productionDirs)
	}
	return out
}

// TestWorkspaceLiteralIsOneNameAndBothRulesUseIt is direction (3). The census below subtracts
// exactly one name; if the two rules stop agreeing on which, that subtraction is silently wrong.
func TestWorkspaceLiteralIsOneNameAndBothRulesUseIt(t *testing.T) {
	ms := workspaceLiteralRe.FindAllStringSubmatch(readRuleFile(t), -1)
	if len(ms) < 2 {
		t.Fatalf("expected at least TWO chi.URLParam($R, \"...\") literals in %s — the sibling rule's "+
			"`pattern` and the indirect rule's `pattern-not` — found %d. The partition between the two "+
			"rules is written in those two lines; if one is gone, one of them now owns nothing or both "+
			"own the same read.", ruleFile, len(ms))
	}
	for _, m := range ms {
		if m[1] != workspaceParam {
			t.Errorf("rule file names %q as a chi.URLParam literal; this guard subtracts %q. "+
				"Either the workspace param was renamed (update workspaceParam AND every route mount) "+
				"or the two rules have drifted apart.", m[1], workspaceParam)
		}
	}
}

// workspaceParam is PINNED, not derived. Deriving it from the rule file would make this guard
// agree with whatever the rule file happens to say, which is the failure it exists to prevent.
const workspaceParam = "wsID"

// TestCensusMatchesTree is directions (1) and (2). It is the one that fails when the exempt set
// and the read set drift apart, in either direction.
func TestCensusMatchesTree(t *testing.T) {
	census := censusNames(t)
	inCensus := map[string]bool{}
	for _, n := range census {
		inCensus[n] = true
	}
	if inCensus[workspaceParam] {
		t.Errorf("%q is in the indirect rule's census, so BOTH rules now exclude it and NOTHING owns "+
			"the workspace param. That is the hole this rule set exists to close.", workspaceParam)
	}

	tree := treeLiterals(t)

	// (1) read but not exempt → the scan reddens on ordinary code.
	for name, site := range tree {
		if name == workspaceParam || inCensus[name] {
			continue
		}
		t.Errorf("param %q is read at %s but is NOT in docs-no-indirect-url-param-scope's census.\n"+
			"  If it is a resource id, add it to the regex in %s (that is the classification, and it is "+
			"reviewed in this diff).\n"+
			"  If it CAN BE THE WORKSPACE, do not add it — authorize it, or scope by "+
			"authz.WorkspaceIDs(ctx). The scan will red on it too, which is the intended outcome.",
			name, site, ruleFile)
	}

	// (2) exempt but not read → a dead exemption, and no scan can see this one.
	for _, name := range census {
		if _, read := tree[name]; !read {
			t.Errorf("param %q is exempted by %s but is READ NOWHERE in %v.\n"+
				"  A name that left the code and kept its pass is a hole waiting for the name to be "+
				"re-used. Delete it from the census in the commit that deleted its last reader.",
				name, ruleFile, productionDirs)
		}
	}

	if !t.Failed() {
		t.Logf("census and tree agree: %d exempt names + %q, all live", len(census), workspaceParam)
	}
}
