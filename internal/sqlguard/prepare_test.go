// Package sqlguard holds the guard that asks POSTGRES, not a regex, whether every SQL statement
// this binary sends names something that exists.
//
// THE CLASS. `go build` and `go vet` cannot see inside a string. A column renamed or dropped by a
// later migration leaves every statement that names it compiling, passing review, shipping, and
// then failing at runtime on whichever code path reaches it first — which for a rarely-taken
// branch can be weeks. Nothing in this repository looked at SQL text against the real schema; the
// existing guards look at Go source against Go source (clockguard, paramcensus, routeguard) or at
// fixtures against handlers (bodyfieldcensus, ai/fixture_keys).
//
// HOW IT MEASURES. Every statement is handed to the database with PREPARE, against the same
// from-zero migrated schema testutil.New builds. Postgres resolves the names or it does not.
// PREPARE plans without executing, so nothing is written and nothing is read.
//
// WHAT IT ASSERTS:
//
//	R1 CLASSIFIED    every pool.Query/QueryRow/Exec call site in the non-test tree is either
//	                 reconstructable from source or listed in unreconstructable with a reason.
//	                 A new call site whose SQL cannot be read is RED until somebody says why.
//	R2 PREPARES      every reconstructable statement PREPAREs against the real schema.
//	R3 FLOOR         at least statementFloor statements are reconstructed. An extractor that
//	                 stops seeing SQL reports a clean tree, not a broken instrument — and this
//	                 one demonstrably can: its first draft read only `const` bindings and could
//	                 not see page/store.go's `var columns`, losing eight statements silently.
//	R4 ORACLE-LIVE   the guard proves IN BAND that its own oracle rejects a bad name, by
//	                 preparing a deliberately wrong statement and requiring the same classifier
//	                 to call it a name error. Without this, a PREPARE step that had silently
//	                 stopped reaching the database would report every statement green.
//	R5 NO STALE      no entry in unreconstructable is actually reconstructable. A stale exemption
//	                 hides a real statement from R2, and nothing else would notice.
//	R6 SCHEMA-REAL   the schema every verdict is measured against is the one migrate.Apply builds,
//	                 and it applied the real number of migrations. A guard that silently checked
//	                 an empty database would prepare nothing and report nothing.
//
// ⚠ WHAT IT CANNOT SEE, STATED RATHER THAN IMPLIED.
//
//   - Statements assembled at runtime. page/store.go's shared projection is
//     `var columns = strings.Join(columnExprs(""), ", ")` — computed by a Go function, so no
//     reader of the source can know its value. Those nine sites, and the Sprintf builders, are
//     in unreconstructable BY NAME. R1 keeps that list honest in one direction and R5 in the
//     other, but a wrong column inside one of them is invisible here.
//   - `.semgrep/tests/` is excluded: it is deliberately-wrong sample code for the semgrep rules,
//     not shipped SQL, and including it would put six statements in the population that this
//     binary never sends.
//   - Whether a statement is CORRECT. It says the names resolve. It does not say the query means
//     what its caller believes.
//
// ⚠ AND WHAT THE CENSUS BEHIND IT RETURNED, so the next session does not redo it: at
// df4a90d the whole thing is CLEAN. 202 columns across 21 tables; every one is named by a
// product statement that touches its own table; every reconstructable statement prepares. This
// guard changes no behaviour. What it adds is that the next one cannot arrive silently.
package sqlguard

import (
	"context"
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

	"github.com/talyvor/docs/internal/migrate"
	"github.com/talyvor/docs/internal/testutil"
	"github.com/talyvor/docs/migrations"
)

// statementFloor is the number of statements the extractor reconstructed at df4a90d. It is a
// FLOOR, not a target: reconstructing fewer means the extractor stopped seeing SQL, which is the
// failure mode that reports a clean tree. Raising it when statements are added is correct;
// lowering it needs a reason in the diff.
const statementFloor = 130

// unreconstructable is the classified list of call sites whose SQL cannot be read from source,
// keyed "path:line" with the reason. R1 fails on a call site that is neither reconstructable nor
// here; R5 fails on an entry that has become reconstructable.
var unreconstructable = map[string]string{
	"internal/approval/store.go:530":   "prefixed(...) builds the projection at runtime",
	"internal/changelog/store.go:247":  "fmt.Sprintf assembles the statement",
	"internal/database/store.go:687":   "fmt.Sprintf assembles the statement",
	"internal/migrate/migrate.go:208":  "string(...) conversion of an embedded migration file",
	"internal/page/store.go:373":       "var columns = strings.Join(columnExprs(\"\"), \", \") — computed by Go",
	"internal/page/store.go:593":       "same shared runtime-computed projection",
	"internal/page/store.go:598":       "same shared runtime-computed projection",
	"internal/page/store.go:631":       "same shared runtime-computed projection",
	"internal/page/store.go:920":       "sql is built by fmt.Sprintf above the call",
	"internal/page/store.go:1269":      "same shared runtime-computed projection",
	"internal/page/store.go:1333":      "same shared runtime-computed projection",
	"internal/page/store.go:1402":      "prefixedColumns(...) builds an aliased projection at runtime",
	"internal/page/store.go:1500":      "same shared runtime-computed projection",
	"internal/space/store.go:180":      "sql is built by fmt.Sprintf above the call (the dynamic SET)",
	"internal/testutil/harness.go:120": "dropDatabaseStmt(...) — DDL against a database name, not the schema",
	"internal/testutil/harness.go:123": "createDatabaseStmt(...) — DDL against a database name, not the schema",
	"internal/testutil/harness.go:172": "string(...) conversion of an embedded migration file",
	"internal/testutil/harness.go:188": "dropDatabaseStmt(...) — DDL against a database name, not the schema",
}

// site is one reconstructed statement.
type site struct {
	key  string // "path:line"
	sql  string
	file string
	line int
}

// repoRoot walks up for go.mod. LOUD when it cannot find one — a guard that silently resolves to
// the wrong tree scans nothing and passes.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod walking up from the test's working directory")
		}
		dir = parent
	}
}

// stringBindings collects every identifier in a file bound to a plain string literal —
// `const x = ...`, `var x = ...`, and `x := ...`. ⚠ THE FIRST DRAFT READ ONLY `const`, and
// page/store.go binds its shared projection with `var`; eight statements vanished from the
// population and the run still printed a clean result. R3's floor is what would say so now.
func stringBindings(f *ast.File) map[string]string {
	out := map[string]string{}
	record := func(names []*ast.Ident, vals []ast.Expr) {
		for i, n := range names {
			if i >= len(vals) {
				return
			}
			if bl, ok := vals[i].(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if s, err := strconv.Unquote(bl.Value); err == nil {
					if _, seen := out[n.Name]; !seen {
						out[n.Name] = s
					}
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.ValueSpec:
			record(d.Names, d.Values)
		case *ast.AssignStmt:
			var names []*ast.Ident
			for _, l := range d.Lhs {
				id, ok := l.(*ast.Ident)
				if !ok {
					return true
				}
				names = append(names, id)
			}
			record(names, d.Rhs)
		}
		return true
	})
	return out
}

// flatten resolves a SQL argument expression to its text, or returns ok=false with the reason it
// could not. It handles string literals, identifiers bound to string literals, and `+` chains of
// those — which is every static statement in this tree.
func flatten(e ast.Expr, binds map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := binds[v.Name]
		return s, ok
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, okL := flatten(v.X, binds)
		r, okR := flatten(v.Y, binds)
		return l + r, okL && okR
	case *ast.ParenExpr:
		return flatten(v.X, binds)
	}
	return "", false
}

var dbCall = map[string]bool{"Query": true, "QueryRow": true, "Exec": true}

// driftSuspects returns one note per file whose R1/R5 failures carry the LINE-DRIFT signature.
//
// ⚠ IT IS DELIBERATELY CONSERVATIVE AND ONLY EVER ADDS A NOTE — it cannot suppress an R1 or an R5,
// and there is no input on which it makes this guard catch less. The signature is: within ONE file,
// N exemptions went orphaned AND exactly N blind sites came up unlisted. Any other shape (only
// orphans, only unlisted, or unequal counts) is left undiagnosed rather than guessed at, because a
// wrong cause printed confidently is worse than no cause at all — which is the failure this whole
// note exists to undo.
func driftSuspects(orphaned map[string][]string, unlisted map[string][]blindSite) []string {
	var files []string
	for f := range orphaned {
		files = append(files, f)
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		orphans, news := orphaned[f], unlisted[f]
		if len(news) == 0 || len(news) != len(orphans) {
			continue
		}
		var moved []string
		for _, b := range news {
			where := b.fn
			if where == "" {
				where = "file scope"
			}
			moved = append(moved, fmt.Sprintf("%s (in %s)", b.key, where))
		}
		out = append(out, fmt.Sprintf(
			"%s%s: %d exemption(s) orphaned (%s) and exactly %d unlisted blind site(s) (%s). "+
				"That is the signature of lines MOVING above these call sites, not of their SQL "+
				"changing — check whether anything above them changed line count before touching "+
				"the statements. ⚠ IF YOU ARE RUNNING A MUTATION HARNESS: make your arms "+
				"LINE-PRESERVING, or this guard reds for free and a red that means nothing will "+
				"be scored as a catch.",
			driftNotePrefix, f, len(orphans), strings.Join(orphans, ", "), len(news),
			strings.Join(moved, ", ")))
	}
	return out
}

// driftNotePrefix is named rather than inlined so the control that proves this note appears for a
// drift — and NOT for a genuine change — has something stable to match on.
const driftNotePrefix = "R1/R5 LINE DRIFT SUSPECTED in "

// keyFile splits "path:line" back to its path. The exemption list is keyed by line and stays that
// way; this only groups orphans per file so a drift signature can be recognised within one file.
func keyFile(key string) string {
	if i := strings.LastIndex(key, ":"); i >= 0 {
		return key[:i]
	}
	return key
}

// funcName renders a FuncDecl the way a human reads it, receiver included, so a blind site can be
// reported as "(*Store).SearchWithRank" rather than as a line number that has already moved.
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	var recv string
	switch rt := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			recv = "*" + id.Name
		}
	case *ast.Ident:
		recv = rt.Name
	}
	if recv == "" {
		return fd.Name.Name
	}
	return "(" + recv + ")." + fd.Name.Name
}

// blindSite is a call site whose SQL could not be read from source. It carries the ENCLOSING
// FUNCTION as well as the line, and that is the whole point of the type: the exemption list is
// keyed by line, so when lines move the key is worthless for telling a human WHICH call site an
// orphaned entry meant. A function name survives a line shift; a line number does not.
type blindSite struct {
	key  string // "path:line" — still the exemption key, unchanged
	file string
	fn   string // enclosing func, e.g. "(*Store).SearchWithRank"; "" at file scope
}

// scan walks the non-test tree and returns the reconstructed statements plus the call sites it
// could not reconstruct.
func scan(t *testing.T, root string) (found []site, blind []blindSite) {
	t.Helper()
	fset := token.NewFileSet()
	var files int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			// .semgrep/tests holds deliberately-wrong sample code; frontend and vendor hold no Go.
			if base == ".git" || base == ".semgrep" || base == "frontend" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		files++
		binds := stringBindings(f)
		rel, _ := filepath.Rel(root, path)
		// enclosing tracks the FuncDecl we are inside. ast.Inspect walks depth-first, so the most
		// recent FuncDecl entered is the one a call belongs to; it is cleared on the way back out.
		var enclosing string
		ast.Inspect(f, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok {
				enclosing = funcName(fd)
				return true
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !dbCall[sel.Sel.Name] || len(call.Args) < 2 {
				return true
			}
			// arg 0 is the context; arg 1 is the SQL.
			if id, ok := call.Args[0].(*ast.Ident); !ok || id.Name != "ctx" {
				return true
			}
			line := fset.Position(call.Pos()).Line
			key := fmt.Sprintf("%s:%d", rel, line)
			sql, ok := flatten(call.Args[1], binds)
			if !ok {
				blind = append(blind, blindSite{key: key, file: rel, fn: enclosing})
				return true
			}
			found = append(found, site{key: key, sql: strings.Join(strings.Fields(sql), " "), file: rel, line: line})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files < 40 {
		t.Fatalf("R3: the walk parsed only %d non-test Go files; the extractor is what is broken, "+
			"not the tree", files)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].key < found[j].key })
	sort.Slice(blind, func(i, j int) bool { return blind[i].key < blind[j].key })
	return found, blind
}

// nameError classifies a PREPARE failure. Only a NAME failure is this guard's business: a type
// the planner cannot infer from `$1` alone is a property of PREPARE, not of the statement.
var nameError = regexp.MustCompile(`(?i)(column .* does not exist|relation ".*" does not exist|` +
	`column reference .* is ambiguous|missing FROM-clause entry|does not have a column)`)

func TestEveryStaticStatementResolvesAgainstTheRealSchema(t *testing.T) {
	root := repoRoot(t)
	found, blind := scan(t, root)

	// R3 FLOOR — before anything else, because every rule below is vacuous without a population.
	if len(found) < statementFloor {
		t.Fatalf("R3: reconstructed %d statements, floor is %d. An extractor that stops seeing SQL "+
			"reports a clean tree rather than a broken instrument", len(found), statementFloor)
	}
	t.Logf("population: %d reconstructed statements, %d unreconstructable call sites",
		len(found), len(blind))

	// R1 CLASSIFIED — a call site whose SQL cannot be read must be named, with a reason.
	blindSet := map[string]bool{}
	unlisted := map[string][]blindSite{} // file -> blind sites with no exemption
	for _, b := range blind {
		blindSet[b.key] = true
		if _, ok := unreconstructable[b.key]; !ok {
			unlisted[b.file] = append(unlisted[b.file], b)
		}
	}
	// R5 NO STALE — an exemption that is no longer needed hides a statement from R2.
	orphaned := map[string][]string{} // file -> exemption keys no longer blind
	for k := range unreconstructable {
		if !blindSet[k] {
			orphaned[keyFile(k)] = append(orphaned[keyFile(k)], k)
		}
	}
	for f := range orphaned {
		sort.Strings(orphaned[f])
	}

	// ⚠ THE LINE-DRIFT DIAGNOSIS, AND IT CHANGES NOTHING ABOUT WHAT THIS GUARD CATCHES — R1 and R5
	// still fail below, on exactly the same inputs. It changes only what the failure SAYS.
	//
	// The exemption list is keyed by "path:line". That is deliberate: editing an exempted call site
	// SHOULD force a human to re-confirm the exemption. But a line key cannot tell "this call site
	// changed" from "something above it changed line count", and those want opposite responses.
	//
	// ⚠ MEASURED, AND IT COST A CENSUS ITS ANSWER (W3.58, tab-p3w8): a mutation harness deleted a
	// three-line block in internal/page/store.go, this guard red with R1 naming :1399 and R5 naming
	// :1402, and BOTH MESSAGES WERE FALSE — nothing about the SQL had changed and no exemption was
	// stale. The harness scored the red as its own arm being CAUGHT, and so reported two UNDEFENDED
	// corrections as DEFENDED. The failure direction is the one that manufactures confidence.
	//
	// The signature is structural and is reported as a CANDIDATE cause, not asserted as fact: in one
	// file, N exemptions went orphaned and N blind sites came up unlisted. The enclosing FUNCTION of
	// each unlisted site is printed because a function name survives a line shift and a line number
	// does not — it is what lets a reader confirm the pairing in one glance.
	for _, note := range driftSuspects(orphaned, unlisted) {
		t.Log(note)
	}

	for f, news := range unlisted {
		for _, b := range news {
			where := b.fn
			if where == "" {
				where = "file scope"
			}
			t.Errorf("R1: %s (in %s) builds its SQL in a way this guard cannot read, and is not in "+
				"unreconstructable. Add it with the reason, or make the statement literal so it "+
				"can be checked", b.key, where)
		}
		_ = f
	}
	for _, orphans := range orphaned {
		for _, k := range orphans {
			t.Errorf("R5: unreconstructable lists %s (%q) but that call site is reconstructable "+
				"now, so the exemption is hiding it from the schema check. Remove the entry",
				k, unreconstructable[k])
		}
	}

	// ⚠ THE SCHEMA IS BUILT BY migrate.Apply ON A BLANK DATABASE — the cmd/docs BOOT PATH — and
	// NOT by testutil.New. They are not the same schema, MEASURED rather than assumed: New applies
	// the embedded .sql files directly and never creates `schema_migrations`, so migrate.go's own
	// two statements read a relation that exists in no test database in this repository. Against
	// New this guard reported them as findings and they are not. Everything else is identical —
	// 202 columns and 66 indexes, byte for byte — so the harness is faithful apart from the ledger,
	// which is worth knowing and was written down nowhere.
	db := testutil.NewBlank(t)
	ctx := context.Background()
	applied, err := migrate.Apply(ctx, db.Pool, migrations.FS)
	if err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	if len(applied) < 20 {
		t.Fatalf("R6: migrate.Apply reported %d migrations applied; the schema this guard checks "+
			"against is not the product's, so every verdict below is about the wrong database",
			len(applied))
	}

	// R4 ORACLE-LIVE — prove the oracle rejects a bad name BEFORE trusting it on 145 good ones.
	// A PREPARE step that had stopped reaching the database, or a classifier that matched
	// nothing, would report every statement green and this is the only rule that would not be.
	_, err = db.Pool.Exec(ctx, `PREPARE sqlguard_oracle AS SELECT no_such_column_xyz FROM pages`)
	if err == nil {
		t.Fatalf("R4: the oracle accepted a column that does not exist — PREPARE is not checking " +
			"names here, so every green below is meaningless")
	}
	if !nameError.MatchString(err.Error()) {
		t.Fatalf("R4: the oracle rejected the bad column but the classifier did not recognise the "+
			"error, so a real name failure would be filed as 'type inference' and ignored: %v", err)
	}

	// R2 PREPARES — the measurement.
	var nameErrs, other int
	for i, s := range found {
		if _, err := db.Pool.Exec(ctx, fmt.Sprintf("PREPARE sqlguard_%d AS %s", i, s.sql)); err != nil {
			if nameError.MatchString(err.Error()) {
				nameErrs++
				t.Errorf("R2: %s names something the schema does not have.\n    err: %v\n    sql: %s",
					s.key, err, s.sql)
				continue
			}
			// Not this guard's business — recorded so the number is visible rather than implied.
			other++
		}
	}
	t.Logf("prepared %d, name errors %d, unprepared for non-name reasons %d",
		len(found)-nameErrs-other, nameErrs, other)

	// ⚠ AND A FLOOR ON THE PREPARES THEMSELVES. If every statement failed for a "non-name reason"
	// the loop above would pass in silence. At df4a90d, 4 of 145 fail type inference.
	if len(found)-nameErrs-other < statementFloor-20 {
		t.Fatalf("only %d statements actually prepared; the rest failed for reasons this guard "+
			"files as 'not my business'. That is a guard checking almost nothing",
			len(found)-nameErrs-other)
	}
}
