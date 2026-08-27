// Package clockguard holds the FRESHNESS-CLOCK guard for every statement in this repository
// that writes the `pages` table.
//
// THE CLASS. `pages.updated_at` is the freshness clock. `page.Store.GetStalePages` keys on it
// (`AND updated_at < NOW() - INTERVAL '1 day' * stale_after_days`) and feeds four shipped
// surfaces: the SPA's stale screen and sidebar count, the 09:00 UTC freshness digest,
// GET /v1/workspaces/{wsID}/stale, and the MCP `get_stale_pages` tool. A write that is not an
// EDIT must therefore not move it — otherwise a document nobody touched silently drops off the
// stale list, and `PageView.tsx` prints "Last edited by {updated_by} · {updated_at}" for an edit
// that never happened, attributed to whoever last really edited it.
//
// THE RULE HAS BEEN BROKEN FIVE TIMES AND FIXED FIVE TIMES, ONE SITE AT A TIME. The source
// comments record each: the RecordView copy (internal/page/store.go, the block above Verify),
// Delete's re-parent (reparent_staleness_realpg_test.go), the depth re-base
// (parentdepth_realpg_test.go), PriceAISpend (aispend_staleness_test.go), and Verify itself
// (verify_editclock_realpg_test.go). TWO of those comments say, in the repository's own words:
//
//	"⚠⚠ IT MUST NOT TOUCH `updated_at`, AND NOTHING BUT THIS SENTENCE STOPS IT."
//
// That sentence was accurate, and it is what this file replaces. Five per-site behavioural tests
// cover five sites; nothing covered the other seven, and nothing at all could see a THIRTEENTH
// writer being added tomorrow — which is the shape every one of the five arrived in.
//
// WHAT THIS GUARD ASSERTS, in the direction that is dangerous:
//
//	R1 CLASSIFIED   every `UPDATE pages` statement in the tree is in pagesWriters, with a verdict.
//	                A new writer is RED until somebody says which kind it is.
//	R2 CLOCK        a NON-EDIT writer's statement must not set `updated_at`.
//	R3 READABLE     a NON-EDIT writer must write its SET clause LITERALLY, because R2 reads that
//	                text and can only judge what it can see. `page.Store.Update` sets the column
//	                through fmt.Sprintf("updated_at = $%d", n) into `UPDATE pages SET %s`, so a
//	                dynamic SET is a demonstrated way to move this clock with nothing in the SQL
//	                literal to read. R3 makes R2's precondition a rule instead of an assumption.
//	                ⚠ ITS FIRST DRAFT WAS WRONG IN THE DIRECTION THAT MATTERS AND RUNNING IT SAID
//	                SO: it flagged any `updated_at = …` literal anywhere in the enclosing
//	                function, and `approval.Decide` writes `UPDATE approval_requests SET status =
//	                $1, updated_at = NOW()` one statement away from its `pages` write. That column
//	                is a DIFFERENT table's and is entirely correct. A guard that reds on a correct
//	                line gets deleted, so the rule is now about the pages statement's own shape.
//	R4 FLOOR        the extractor must find at least writerFloor statements. An extractor that
//	                stops seeing SQL reports a clean tree, not a broken instrument.
//
// and in the direction that keeps it from being satisfiable by doing nothing:
//
//	R5 EDIT-LIVE    the EDIT writer must actually set the clock. If `page.Store.Update` stops
//	                bumping updated_at, every page's freshness clock freezes at creation and the
//	                stale list fills with documents that were edited this morning. R1–R4 are all
//	                green in that world; R5 is the only rule that is not.
//
// WHAT IT CHANGES: NOTHING, DELIBERATELY. All thirteen sites already satisfy R1-R5 at
// 2c69812; this guard was green on its first run and every rule in it was therefore
// positive-controlled before it was believed (harness
// ~/talyvor-queue/w312-clockguard-controls-n8k4.py). What it adds is that the FOURTEENTH writer
// cannot arrive silently, and that the seven sites no test covered are covered by something.
//
// ⚠ WHAT IT CANNOT SEE, STATED RATHER THAN IMPLIED:
//
//	· It reads Go source. A database trigger, or SQL executed from outside this binary, is
//	  invisible to it. Measured at 2c69812: no file in migrations/ writes `pages` at all.
//	· It excludes _test.go on purpose — fixtures age a page by writing updated_at directly
//	  (sec4_workspace_routes_test.go does exactly that), and that is how they make a page stale.
//	· It folds only STRING LITERALS. A statement assembled from a non-literal fragment would be
//	  partly invisible; today every one of the thirteen is a literal, and R4's floor is what
//	  notices if that stops being true.
package clockguard

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

// verdict is what a writer is ALLOWED to do to the freshness clock.
type verdict int

const (
	// nonEdit — the statement changes something about a page that is not its content:
	// bookkeeping, a status, a lock, a counter, a cost. The reader did not edit anything, so the
	// clock must not move.
	nonEdit verdict = iota
	// edit — the statement IS the edit. It must set updated_at; that is the clock's only writer.
	edit
)

// pagesWriters is the PINNED CLASSIFICATION of every statement in the tree that writes `pages`.
//
// Keyed by "<package> <Receiver>.<Func>" — the enclosing function, not the file:line, because a
// line number decays on the next unrelated edit and this guard would then be repaired rather than
// consulted.
//
// ⚠ DERIVED BY RUNNING THE EXTRACTOR, NOT WRITTEN FROM A GREP. The `why` on each entry is the
// argument for its verdict and is the thing to re-read when one of them changes.
var pagesWriters = map[string]struct {
	v   verdict
	why string
}{
	"page (*Store).Update": {edit,
		"THE edit. Sets updated_at through the dynamic SET builder (fmt.Sprintf(\"updated_at = $%d\")), " +
			"which is why R3 reads Go string literals and not only SQL text. R5 pins that it still does."},
	"page (*Store).rebaseSubtreeDepth": {nonEdit,
		"a subtree's depth column after somebody moved its ROOT. These rows belong to pages nobody edited."},
	"page (*Store).Delete": {nonEdit,
		"Delete's re-parent: children re-pointed at their grandparent because their PARENT was deleted. " +
			"Those children are somebody else's document and nobody edited them."},
	"page (*Store).Verify": {nonEdit,
		"\"this is still accurate\" — an explicit claim that NOTHING CHANGED. Recording it as an edit " +
			"contradicts the thing being recorded, and it made freshness/engine.go#buildReport's " +
			"verify-wins branch unreachable (NOW() is one value per transaction, After is strict)."},
	"page (*Store).UpdateAICost": {nonEdit,
		"the linked-issue cost sweep. Server-side bookkeeping, no user, no content change."},
	"page (*Store).PriceAISpendWithServeSource": {nonEdit,
		"lands a Lens price on a page minutes-to-hours after the call. summarize and title-suggest " +
			"are read-only, and the sweep's clock is not the operation's clock."},
	"analytics (*Store).RecordView": {nonEdit,
		"reading a page is not editing it. The dead copy of this bump in internal/page is the seam " +
			"every other comment in this class refers back to."},
	"pagelock (*Store).Lock": {nonEdit,
		"taking an edit lock is an announcement of intent, not an edit. The content is unchanged."},
	"pagelock (*Store).Unlock": {nonEdit,
		"releasing a lock. If the holder edited anything, Update already moved the clock."},
	"approval (*Store).RequestApproval": {nonEdit,
		"doc_status transition. Approval is a claim ABOUT the content, so moving the clock would " +
			"reset the freshness window on a document precisely when it was confirmed still-good."},
	"approval (*Store).Decide": {nonEdit,
		"doc_status transition, same argument as RequestApproval."},
	"approval (*Store).PublishApproved": {nonEdit,
		"doc_status transition, workspace-scoped."},
	"approval (*Store).SetStatus": {nonEdit,
		"doc_status transition, workspace-scoped."},
}

// writerFloor is a PINNED FLOOR on the discovered population.
//
// WHY A FLOOR AND NOT AN EQUALITY: an exact pin reds on every ordinary new writer, which is how a
// pin becomes a rubber stamp — and R1 already reds on a new writer, by name, with a better
// message. What the floor is for is the opposite failure: the extractor going blind. A parser
// that finds zero `UPDATE pages` statements satisfies R1, R2 and R3 vacuously and reports a clean
// tree. Control F1 is the record that this is not decoration.
const writerFloor = 13

// updatePages matches the statement this guard is about. `UPDATE pages p` (an aliased target, as
// in PriceAISpendWithServeSource) and a newline between the keyword and the table both count.
var updatePages = regexp.MustCompile(`(?is)\bUPDATE\s+pages\b`)

// clockAssign matches an assignment TO the clock, in SQL or in a Go-built SET fragment:
// `updated_at = NOW()`, `updated_at=$3`, `updated_at = $%d`. It deliberately does NOT match a
// bare mention — `ORDER BY updated_at`, `SELECT … updated_at`, and the `columns` list are all
// reads, and a rule that reddened on them would be tuned off within a week.
var clockAssign = regexp.MustCompile(`(?i)\bupdated_at\s*=`)

// dynamicSet matches a `pages` UPDATE whose SET clause is a format verb rather than columns —
// `UPDATE pages SET %s WHERE id = $%d`. That is the shape page.Store.Update uses, and the only
// shape in which R2 can read a statement and still be wrong about what it sets.
var dynamicSet = regexp.MustCompile(`(?is)\bUPDATE\s+pages\s+(?:\w+\s+)?SET\s+%`)

type site struct {
	key  string // "<package> <Receiver>.<Func>"
	file string
	line int
	stmt string // the SQL literal, from `UPDATE pages` onward
	// dynamicSet: the statement's SET clause starts with a format verb (`UPDATE pages SET %s`),
	// so what it actually sets is assembled in Go and R2 cannot read it.
	dynamicSet bool
	// buildsClock: the enclosing function builds an `updated_at = …` fragment in Go. Only
	// consulted for the EDIT writer (R5) — for a NON-EDIT writer R3 has already refused the
	// dynamic SET that would be needed to deliver it, and the fragment on its own says nothing
	// about which table it targets.
	buildsClock bool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/clockguard -> repo root
}

func recvTypeName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	e := fd.Recv.List[0].Type
	star := ""
	if s, ok := e.(*ast.StarExpr); ok {
		e, star = s.X, "*"
	}
	if id, ok := e.(*ast.Ident); ok {
		return "(" + star + id.Name + ")"
	}
	return ""
}

// stringLits returns every string literal inside a function body, unquoted.
func stringLits(fd *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fd, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(bl.Value)
		if err != nil {
			return true
		}
		out = append(out, s)
		return true
	})
	return out
}

// collect walks every non-test .go file in the repository and returns one entry per statement
// that writes `pages`.
//
// ⚠ IT FAILS RATHER THAN SKIPS on a file it cannot parse. A guard that quietly drops an
// unparseable file reports a smaller population than the tree has, and the floor below would then
// be measuring the parser rather than the code.
func collect(t *testing.T, root string) []site {
	t.Helper()
	var out []site
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// ⚠ EVERY DOT-DIRECTORY IS SKIPPED, AND `.semgrep` IS WHY IT IS A CLASS AND NOT A
			// NAME. .semgrep/tests/*.go are RULE FIXTURES: deliberate violations that exist so
			// `semgrep --test` can prove each rule still catches what it claims. This guard's
			// first run reported eight of them as unclassified `pages` writers — correctly, by
			// its own definition, and uselessly. Pinning them would have made this file assert
			// things about another guard's fixtures.
			if info.Name() != "." && strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			switch info.Name() {
			case "node_modules", "frontend", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("clockguard: parse %s: %v", path, perr)
		}
		pkg := f.Name.Name
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			lits := stringLits(fd)
			buildsClock := false
			for _, l := range lits {
				if clockAssign.MatchString(l) && !updatePages.MatchString(l) {
					buildsClock = true
				}
			}
			name := fd.Name.Name
			if r := recvTypeName(fd); r != "" {
				name = r + "." + name
			}
			for _, l := range lits {
				loc := updatePages.FindStringIndex(l)
				if loc == nil {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				out = append(out, site{
					key:         pkg + " " + name,
					file:        rel,
					line:        fset.Position(fd.Pos()).Line,
					stmt:        l[loc[0]:],
					dynamicSet:  dynamicSet.MatchString(l),
					buildsClock: buildsClock,
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("clockguard: walk: %v", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out
}

func TestFreshnessClock_EveryPagesWriterIsClassifiedAndOnlyTheEditMovesTheClock(t *testing.T) {
	root := repoRoot(t)
	sites := collect(t, root)

	// R4 FLOOR — before anything else, because R1/R2/R3 are all vacuously green on an empty set.
	if len(sites) < writerFloor {
		t.Fatalf("R4 FLOOR: found %d `UPDATE pages` statements, want at least %d.\n"+
			"Either writers were deleted (update writerFloor deliberately, in the same commit that "+
			"deletes them) or this extractor has stopped seeing SQL. The second is the dangerous one: "+
			"every other rule in this file passes on an empty set.",
			len(sites), writerFloor)
	}

	// The population, reported on every run. A guard that only speaks when it is angry gives a
	// reader no way to tell "nothing is wrong" from "nothing was looked at" — which is the exact
	// failure R4 exists for, and the exact reason `go test -v ./internal/clockguard/` should show
	// thirteen lines rather than one word.
	for _, s := range sites {
		kind := "NON-EDIT"
		if w, ok := pagesWriters[s.key]; ok && w.v == edit {
			kind = "EDIT    "
		}
		t.Logf("  %s  %-46s %s:%d", kind, s.key, s.file, s.line)
	}
	t.Logf("clockguard: %d `UPDATE pages` statements classified (floor %d)", len(sites), writerFloor)

	for _, s := range sites {
		w, known := pagesWriters[s.key]
		if !known {
			// R1 CLASSIFIED.
			t.Errorf("R1 CLASSIFIED: %s (%s:%d) writes `pages` and is not in pagesWriters.\n"+
				"  statement: %s\n"+
				"  Say which it is. A NON-EDIT writer must not set updated_at — GetStalePages keys on "+
				"that column and feeds the stale screen, the daily digest, GET /workspaces/{wsID}/stale "+
				"and the MCP get_stale_pages tool. If this really is an edit, add it as `edit` and say "+
				"why in the same line.",
				s.key, s.file, s.line, strings.Join(strings.Fields(s.stmt), " "))
			continue
		}
		if w.v == edit {
			continue
		}
		// R2 CLOCK — the SET clause of the statement itself.
		if clockAssign.MatchString(s.stmt) {
			t.Errorf("R2 CLOCK: %s (%s:%d) is classified NON-EDIT and its statement sets updated_at.\n"+
				"  why it is non-edit: %s\n"+
				"  statement: %s\n"+
				"  This is the sixth copy of a seam this repository has already fixed five times.",
				s.key, s.file, s.line, w.why, strings.Join(strings.Fields(s.stmt), " "))
		}
		// R3 READABLE — the same defect assembled in Go instead of written in SQL.
		if s.dynamicSet {
			t.Errorf("R3 READABLE: %s (%s:%d) is classified NON-EDIT and its SET clause is built "+
				"in Go, so R2 cannot read what it sets.\n"+
				"  why it is non-edit: %s\n"+
				"  statement: %s\n"+
				"  Write the columns literally, or reclassify it and argue for the clock bump. "+
				"page.Store.Update is the one writer allowed a dynamic SET; see R5.",
				s.key, s.file, s.line, w.why, strings.Join(strings.Fields(s.stmt), " "))
		}
	}
}

// R5 EDIT-LIVE — the rule that keeps the other four from being satisfiable by a tree in which
// NOTHING moves the clock. R1-R4 are all rules against setting updated_at; on their own they are
// happiest in a repository where no statement ever does.
//
// ⚠ WHAT IT IS *NOT*, MEASURED RATHER THAN ASSUMED, BECAUSE THE FIRST DRAFT OF THIS COMMENT
// CLAIMED OTHERWISE. It is not the only thing that sees the edit clock stop. Control C5 removed
// the bump from page.Store.Update and ran the WHOLE suite: 8 packages red, ~50 tests, including
// TestUpdate_PersistsPageType, the whole version-history set and every collab persistence test.
// The behavioural cover there is thorough and R5 adds nothing to it.
//
// What R5 actually covers is the case C4 produces: the EXTRACTOR going blind. With the statement
// regex matching nothing, R1/R2/R3 iterate over an empty set and pass with a real defect sitting
// in the tree; R4 catches the count and R5 independently catches that the edit writer has gone
// missing. Two rules on one failure is the point — R4's floor is a number somebody could lower.
func TestFreshnessClock_TheEditWriterStillMovesTheClock(t *testing.T) {
	root := repoRoot(t)
	sites := collect(t, root)

	var editKeys []string
	for k, w := range pagesWriters {
		if w.v == edit {
			editKeys = append(editKeys, k)
		}
	}
	sort.Strings(editKeys)
	if len(editKeys) == 0 {
		t.Fatalf("R5 EDIT-LIVE: pagesWriters classifies nothing as `edit`. Some statement has to " +
			"move the freshness clock or GetStalePages reports every page forever.")
	}

	for _, key := range editKeys {
		var found, moves bool
		for _, s := range sites {
			if s.key != key {
				continue
			}
			found = true
			if clockAssign.MatchString(s.stmt) || (s.dynamicSet && s.buildsClock) {
				moves = true
			}
		}
		if !found {
			t.Errorf("R5 EDIT-LIVE: %s is pinned as the edit writer and no `UPDATE pages` statement "+
				"was found in it. Either it was renamed (fix the key) or the edit path no longer "+
				"writes pages at all.", key)
			continue
		}
		if !moves {
			t.Errorf("R5 EDIT-LIVE: %s is the edit writer and it no longer sets updated_at.\n"+
				"  Every page's freshness clock now freezes at creation: GetStalePages requires "+
				"updated_at past the TTL, so a document edited this morning stays on the stale list "+
				"forever and the SPA, the digest, /workspaces/{wsID}/stale and the MCP tool all "+
				"report it.", key)
		}
	}
}
