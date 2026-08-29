package sqlguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// THE LINE-DRIFT NOTE IS ITSELF A GUARD AND NEEDS ONE, BECAUSE ITS SUBJECT IS A `t.Log`.
//
// `TestEveryStaticStatementResolvesAgainstTheRealSchema` reds identically with and without this
// note — the note only explains WHY. So nothing in a normal run would notice if it stopped being
// emitted, or started being emitted for the wrong shape. This file is what notices.
//
// ⚠ WHY THE NOTE EXISTS AT ALL, measured rather than supposed (W3.58, tab-p3w8): a mutation
// harness deleted a three-line block in internal/page/store.go; the SQL guard red with
// `R1: internal/page/store.go:1399 builds its SQL in a way this guard cannot read` and
// `R5: unreconstructable lists internal/page/store.go:1402 ... but that call site is
// reconstructable now`. BOTH STATEMENTS WERE FALSE. No SQL had changed and no exemption was stale;
// three lines had moved above two exempted call sites. The harness scored that red as its own arm
// being CAUGHT and therefore reported two UNDEFENDED corrections as DEFENDED. The failure
// direction is the one that manufactures confidence, which is why a wrong cause stated
// confidently is worse here than no cause at all.
//
// ⚠ THE ASYMMETRY IS THE WHOLE TEST. A note that fired on every R1/R5 would be worthless — it
// would relabel every genuine finding as line drift, which is the same defect pointing the other
// way. So the cases below assert it fires for the drift shape and STAYS SILENT for a genuinely
// new unreadable statement, a genuinely stale exemption, and any count mismatch.
func TestDriftSuspects_FiresOnlyOnTheDriftSignature(t *testing.T) {
	site := func(key, file, fn string) blindSite {
		return blindSite{key: key, file: file, fn: fn}
	}
	const f = "internal/page/store.go"

	cases := []struct {
		name     string
		orphaned map[string][]string
		unlisted map[string][]blindSite
		wantNote bool
		why      string
	}{
		{
			name:     "the drift signature: N orphaned, exactly N unlisted, same file",
			orphaned: map[string][]string{f: {f + ":1402", f + ":1500"}},
			unlisted: map[string][]blindSite{f: {
				site(f+":1399", f, "(*Store).SearchWithRank"),
				site(f+":1497", f, "(*Store).Search"),
			}},
			wantNote: true,
			why:      "this is the exact shape W3.58 hit, reduced to its inputs",
		},
		{
			name:     "one for one is still the signature",
			orphaned: map[string][]string{f: {f + ":593"}},
			unlisted: map[string][]blindSite{f: {site(f+":590", f, "(*Store).List")}},
			wantNote: true,
			why:      "the smallest real drift is a single shifted call site",
		},
		{
			name:     "a genuinely NEW unreadable statement — no exemption went orphaned",
			orphaned: map[string][]string{},
			unlisted: map[string][]blindSite{f: {site(f+":1399", f, "(*Store).SearchWithRank")}},
			wantNote: false,
			why: "someone made a statement unreadable. Calling that line drift would send the " +
				"reader looking for a line-count change that never happened",
		},
		{
			name:     "a genuinely STALE exemption — nothing came up unlisted",
			orphaned: map[string][]string{f: {f + ":1402"}},
			unlisted: map[string][]blindSite{},
			wantNote: false,
			why: "an exempted statement became literal, which is R5 working exactly as designed " +
				"and is the one case where the entry really should be removed",
		},
		{
			name:     "counts do not match — left undiagnosed rather than guessed",
			orphaned: map[string][]string{f: {f + ":1402", f + ":1500"}},
			unlisted: map[string][]blindSite{f: {site(f+":1399", f, "(*Store).SearchWithRank")}},
			wantNote: false,
			why: "two orphans and one new site is not a clean shift; something else happened too, " +
				"and a confident wrong cause is what this note exists to undo",
		},
		{
			name:     "different files do not pair up",
			orphaned: map[string][]string{f: {f + ":1402"}},
			unlisted: map[string][]blindSite{
				"internal/space/store.go": {site("internal/space/store.go:180", "internal/space/store.go", "(*Store).Update")},
			},
			wantNote: false,
			why:      "a shift is within one file; pairing across files would invent a relationship",
		},
		{
			name:     "nothing wrong at all",
			orphaned: map[string][]string{},
			unlisted: map[string][]blindSite{},
			wantNote: false,
			why:      "the must-stay-silent floor — a note on a clean tree would be noise on every run",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := driftSuspects(tc.orphaned, tc.unlisted)
			if tc.wantNote && len(got) != 1 {
				t.Fatalf("got %d notes, want exactly 1 — %s", len(got), tc.why)
			}
			if !tc.wantNote && len(got) != 0 {
				t.Fatalf("got %d notes, want none — %s:\n%s", len(got), tc.why, strings.Join(got, "\n"))
			}
			if !tc.wantNote {
				return
			}
			note := got[0]
			if !strings.HasPrefix(note, driftNotePrefix) {
				t.Fatalf("note does not carry the prefix a reader greps for:\n%s", note)
			}
			// THE NOTE MUST NAME THE ENCLOSING FUNCTION, which is the only part of a blind site
			// that survives the very line shift being diagnosed. A note that printed the moved
			// line numbers alone would repeat the two numbers the reader already distrusts.
			for _, b := range tc.unlisted[f] {
				if !strings.Contains(note, b.fn) {
					t.Fatalf("note does not name %q, so it repeats the line numbers the drift "+
						"already invalidated and adds nothing:\n%s", b.fn, note)
				}
			}
			// AND IT MUST CARRY THE HARNESS WARNING. That sentence is the entire reason this note
			// was written: the reader who most needs it is running a mutation arm and about to
			// score a free red as a catch.
			if !strings.Contains(note, "LINE-PRESERVING") {
				t.Fatalf("note omits the mutation-harness warning, which is what it is FOR:\n%s", note)
			}
		})
	}
}

// funcName is what makes the note survive a line shift, so its receiver rendering is pinned here
// rather than left to the one caller.
func TestFuncName_RendersReceiversAHumanCanMatch(t *testing.T) {
	for _, tc := range []struct{ src, want string }{
		{"package p\nfunc (s *Store) SearchWithRank() {}", "(*Store).SearchWithRank"},
		{"package p\nfunc (s Store) Search() {}", "(Store).Search"},
		{"package p\nfunc Apply() {}", "Apply"},
	} {
		if got := funcNameFromSrc(t, tc.src); got != tc.want {
			t.Fatalf("funcName(%q) = %q, want %q", tc.src, got, tc.want)
		}
	}
}

// funcNameFromSrc parses one declaration and returns funcName's rendering of it.
func funcNameFromSrc(t *testing.T, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			return funcName(fd)
		}
	}
	t.Fatalf("no FuncDecl in %q", src)
	return ""
}
