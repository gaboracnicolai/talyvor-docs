package page

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// search_clamp_coupling_test.go — FOUR PACKAGES SAY THESE NUMBERS "CANNOT SILENTLY DISAGREE",
// AND NOTHING COMPARED THEM.
//
// Store.Search clamps a caller's limit to 100; Store.SearchWithRank clamps to 50. Four
// constants in three other packages are DERIVED from those two numbers, and three of them say
// so in the same words:
//
//	internal/page/handler.go    searchMaxFetchRows = 100  "the store's OWN ceiling (Search
//	                                                       clamps limit to 100), named here so
//	                                                       the two numbers cannot silently
//	                                                       disagree"
//	internal/mcp/server.go      searchMaxFetchRows = 50   "...SearchWithRank clamps limit to
//	                                                       50... the REST twin's ceiling is 100
//	                                                       because Store.Search clamps at 100"
//	internal/search/handler.go  maxFetchRows       = 50   "...SearchWithRank clamps to 50, named
//	                                                       here so the two numbers cannot
//	                                                       silently disagree"
//	internal/ai/handler.go      askFetchRows       = 12   "12 is inside page.Store.Search's own
//	                                                       clamp of 100, so the number the store
//	                                                       is given is the number it uses"
//
// ⚠ NAMING A NUMBER IS NOT COMPARING IT. "Named here so the two numbers cannot silently
// disagree" describes an intention; the mechanism was never built. This file is the mechanism.
//
// ⚠⚠ AND THE HALF THAT WAS ACTUALLY EXPOSED WAS MEASURED, NOT ASSUMED (W3.53, tab-k4m7,
// ~/talyvor-queue/w353-docs-reach-k4m7.py): SearchWithRank's clamp of 50 IS defended
// (TestSearchWithRank_ClampsLimit reds when it is removed), and **Store.Search's clamp of 100
// is NOT** — remove it and the whole suite stays green. So two of the four citations were
// anchored to a defended number and two to an undefended one, and nothing said which.
//
// ⚠ THE CLAMPS BELOW ARE OBSERVED, NOT RE-READ. The value is taken from the LIMIT argument the
// store actually passes to the query, through pgxmock — so this cannot be satisfied by a
// literal that agrees with itself. The citations, by contrast, are read from their own source,
// which is where they live.
//
// ⚠ AND THE CITATIONS ARE PARSED, NOT GREPPED, FOR A REASON THIS FILE WOULD OTHERWISE HIT: the
// citing files contain the numbers 50 and 100 IN THEIR COMMENTS, several times. A regex for
// `= (\d+)` near the name would happily match prose. go/ast does not see comments at all.

// capturedInt is a pgxmock argument that records the value it was given and always matches.
// It is how the clamp is OBSERVED rather than restated.
type capturedInt struct{ got *int }

func (c capturedInt) Match(v any) bool {
	n, ok := v.(int)
	if ok {
		*c.got = n
	}
	return ok
}

// observedSearchClamp asks Store.Search for an absurd limit and reports what the store
// actually put in the query.
func observedSearchClamp(t *testing.T) int {
	t.Helper()
	store, pool := newMockStore(t)
	var got int
	pool.ExpectQuery(`FROM pages`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), capturedInt{&got}).
		WillReturnRows(pgxmock.NewRows(pageCols()))
	if _, err := store.Search(context.Background(), "ws-1", "x", 9999); err != nil {
		t.Fatalf("Search: %v", err)
	}
	return got
}

// observedSearchWithRankClamp does the same for the ranked twin.
func observedSearchWithRankClamp(t *testing.T) int {
	t.Helper()
	store, pool := newMockStore(t)
	var got int
	cols := append(pageCols(), "space_name", "rank", "headline")
	pool.ExpectQuery(`ts_rank`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), capturedInt{&got}, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(cols))
	if _, err := store.SearchWithRank(context.Background(), "ws-1", "x", nil, 9999, 0); err != nil {
		t.Fatalf("SearchWithRank: %v", err)
	}
	return got
}

func TestSearchClamps_BiteAtTheCeilingsOtherPackagesCite(t *testing.T) {
	if got := observedSearchClamp(t); got != 100 {
		t.Errorf("Store.Search was asked for 9999 and passed %d to the query, want 100. Three "+
			"other packages size their own fetch windows from this number", got)
	}
	if got := observedSearchWithRankClamp(t); got != 50 {
		t.Errorf("Store.SearchWithRank was asked for 9999 and passed %d to the query, want 50", got)
	}
}

// citation is a constant in another package that is documented as derived from one of the
// clamps above. `mustNotExceed` is the clamp it cites; `equals` is set when the constant is
// meant to BE the clamp rather than merely fit inside it.
type citation struct {
	file   string
	name   string
	cites  string // which clamp, for the failure message
	equals int    // 0 ⇒ only the "must not exceed" bound applies
	bound  func(searchClamp, rankClamp int) int
}

func TestSearchClampCitations_StillAgreeWithTheClamps(t *testing.T) {
	search := observedSearchClamp(t)
	rank := observedSearchWithRankClamp(t)

	citations := []citation{
		{filepath.Join(".", "handler.go"), "searchMaxFetchRows", "Store.Search (100)", search,
			func(s, r int) int { return s }},
		{filepath.Join("..", "mcp", "server.go"), "searchMaxFetchRows", "Store.SearchWithRank (50)", rank,
			func(s, r int) int { return r }},
		{filepath.Join("..", "search", "handler.go"), "maxFetchRows", "Store.SearchWithRank (50)", rank,
			func(s, r int) int { return r }},
		{filepath.Join("..", "ai", "handler.go"), "askFetchRows", "Store.Search (100), which it must fit INSIDE", 0,
			func(s, r int) int { return s }},
	}

	found := 0
	for _, c := range citations {
		got, ok := constValue(t, c.file, c.name)
		if !ok {
			t.Errorf("%s no longer declares %s. It was a documented citation of %s; if the "+
				"constant was renamed or removed, update this table in the SAME commit — a "+
				"citation that resolves to nothing is a coupling nobody is checking",
				c.file, c.name, c.cites)
			continue
		}
		found++
		limit := c.bound(search, rank)
		if c.equals != 0 && got != c.equals {
			t.Errorf("%s: %s = %d, but %s is %d. Its own comment says the two numbers "+
				"\"cannot silently disagree\" — this is the comparison that makes that true",
				c.file, c.name, got, c.cites, c.equals)
		}
		if got > limit {
			t.Errorf("%s: %s = %d, which is ABOVE %s (%d). The store will clamp it, so this "+
				"package would be asking for a window it cannot get and sizing its own logic "+
				"on a number that never arrives", c.file, c.name, got, c.cites, limit)
		}
	}

	// NON-VACUITY FLOOR. Every assertion above is a loop over a table, and a loop over rows
	// that all failed to resolve reports nothing. Measured at the commit that introduced this
	// file: 4 citations, all resolving.
	if found < len(citations) {
		t.Fatalf("only %d of %d citations resolved; the rest are reported above. A coupling "+
			"test that cannot find its subjects passes for the wrong reason", found, len(citations))
	}
}

// constValue parses a Go file and returns the value of a named integer constant.
//
// ⚠ IT PARSES RATHER THAN GREPS, and that is load-bearing here rather than stylistic: every
// one of these files states the clamp values IN PROSE, more than once. `mcp/server.go` alone
// writes both 50 and 100 in its comment. A regex would match the documentation instead of the
// declaration and could agree with itself while the code disagreed.
//
// ⚠⚠ IT ALSO EVALUATES, NOT JUST READS, AND THAT WAS NOT FORESIGHT — the first version handled
// only literals and this file's own non-vacuity floor failed it on the spot: `ai/handler.go`
// declares `askFetchRows = askContextPages * askFetchFactor`, a COMPUTED constant, so the
// citation resolved to nothing and 3 of 4 rows were silently unchecked until the floor said so.
// That is the floor earning its place before the guard had shipped.
func constValue(t *testing.T, file, name string) (int, bool) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	// env is every integer const declared in the file, so a constant defined in terms of its
	// neighbours resolves the way the compiler would.
	env := map[string]ast.Expr{}
	ast.Inspect(f, func(n ast.Node) bool {
		gd, isDecl := n.(*ast.GenDecl)
		if !isDecl || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			if vs, isVal := spec.(*ast.ValueSpec); isVal {
				for i, ident := range vs.Names {
					if i < len(vs.Values) {
						env[ident.Name] = vs.Values[i]
					}
				}
			}
		}
		return true
	})
	expr, ok := env[name]
	if !ok {
		return 0, false
	}
	return evalConst(expr, env, 0)
}

// evalConst evaluates the small constant expressions these declarations actually use: an
// integer literal, a reference to another constant in the same file, and + - * over those.
// Anything else is reported as unresolved rather than guessed at — a coupling test that
// invents a value is worse than one that admits it cannot read the file.
func evalConst(e ast.Expr, env map[string]ast.Expr, depth int) (int, bool) {
	if depth > 8 { // a const cycle is a compile error, but this test must not hang on one
		return 0, false
	}
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.Atoi(v.Value)
		return n, err == nil
	case *ast.Ident:
		next, ok := env[v.Name]
		if !ok {
			return 0, false
		}
		return evalConst(next, env, depth+1)
	case *ast.ParenExpr:
		return evalConst(v.X, env, depth+1)
	case *ast.BinaryExpr:
		l, lok := evalConst(v.X, env, depth+1)
		r, rok := evalConst(v.Y, env, depth+1)
		if !lok || !rok {
			return 0, false
		}
		switch v.Op {
		case token.ADD:
			return l + r, true
		case token.SUB:
			return l - r, true
		case token.MUL:
			return l * r, true
		}
	}
	return 0, false
}
