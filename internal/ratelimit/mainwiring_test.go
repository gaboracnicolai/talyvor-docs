package ratelimit_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// THE TRIPWIRE ON THE THREE LINES THAT ARM THE LLM SPEND CEILING — AND IT IS THE LOAD-BEARING
// MEMBER OF THIS REPOSITORY'S mainwiring FAMILY, NOT A FOURTH CHEAP COPY.
//
// ⚠⚠ WHAT WAS MEASURED, NOT ARGUED, AT `a0667e4`: all three `WithRateLimit(...)` calls were
// deleted from cmd/docs/main.go and the ENTIRE gauntlet stayed green — `gofmt -l`, `go vet`,
// `go build`, and `go test -timeout 600s -race -count=1 ./...` against a real Postgres, 36
// packages, EXIT 0. Nothing in this repository could see Docs' only defence against unbounded
// Lens spend being switched off, because every test of every one of the three surfaces builds
// its own handler and hands it its own limiter.
//
// ⚠⚠ AND THE NIL CASE IS FAIL-OPEN ON ALL THREE, WHICH IS WHAT MAKES THE DELETION SILENT RATHER
// THAN LOUD. This is the property that separates this guard from its siblings:
//
//   - internal/analytics' tripwire says of itself that the deleted line "is not silent in
//     production, which is why this is cheap rather than load-bearing" — GetWorkspaceStats
//     returns ErrNoPageReadGate and the screen fails visibly.
//   - internal/mcp's WithAccess is the same shape: a nil AccessController DENIES every write and
//     every gated read, so dropping it is a loud total refusal.
//   - THE LIMITER IS THE OTHER DIRECTION. `mcp.Server.callTool` guards with
//     `llmTools[name] && s.limit != nil && !s.limit.Allow(...)`, so a nil limiter skips the whole
//     condition; `search.Handler.Mount` falls through to a bare `r.Get(...)` with no middleware;
//     `ai.Handler.limited` returns the unwrapped handler. Each convention is documented and each
//     is correct for a bare-handler test — the cost is that production's ceiling is three lines
//     in one file and every one of them fails OPEN.
//
// MEASURED CONSEQUENCE on the sharpest of the three, driven through the shipped gatewayauth+authz
// chain on real Postgres: a server built WITHOUT WithRateLimit answered **50 of 50** `ask_docs`
// calls, against internal/mcp/sec_ratelimit_test.go's limiter, which refuses the 3rd of a burst
// of 2. `ask_docs` reaches Lens on Docs's single service key, and an agent loop calls it far
// faster than a human clicks — the sentence main.go already carries at that call site.
//
// ⚠⚠ RULE B MATCHES A CONSTRUCTOR AND A METHOD IN ONE STATEMENT, NOT A `handler.WithRateLimit(`
// LITERAL, AND THE FIRST DRAFT OF THIS FILE PROVES WHY. It pinned `aiHandler.WithRateLimit(`,
// `searchHandler.WithRateLimit(` and `mcpServer.WithRateLimit(` — the three variable names, read
// off the assignment. NONE OF THOSE THREE STRINGS IS IN main.go: every site is a method CHAIN on
// the constructor's return (`ai.NewHandler(…).WithRateLimit(aiLimiter)`), and the MCP one is
// split across two lines. That draft failed on the healthy tree as loudly as on the broken one,
// so it was caught by its own control run rather than by review — but a literal that had happened
// to match the variable name would have rotted the moment anyone renamed the variable, and rotted
// in the direction of a permanent red. The constructor is the stable name.
//
// ⚠ WHAT THIS GUARD IS AND IS NOT, in the words its siblings established. It reads the deployed
// main, so it catches a wiring call being deleted or moved off the constructor's chain — the
// failure mode a nil-means-unthrottled convention actually has. It CANNOT see a wrong argument (a
// surface handed a limiter built for another one — and note ai and mcp deliberately SHARE
// aiLimiter today), it CANNOT see a handler constructed somewhere else and mounted, and rule A is
// keyed on the METHOD NAME, so a future surface that takes its ceiling some other way
// (`r.Use(limiter.WorkspaceLimit(...))` written directly in main.go, say) is outside it. Those
// residuals are reviewable diffs; a deleted wiring line was not.
//
// ⚠ RULE A EXISTS BECAUSE RULE B CANNOT NOTICE A SURFACE IT WAS NEVER TOLD ABOUT. A guard that
// lists three surfaces reports a clean product forever after a FOURTH is added and left unwired —
// the same silent narrowing internal/testutil/frontendmanifest_test.go and the semgrep
// rule-scope check exist for one layer down. Rule A recomputes the POPULATION from the tree on
// every run and fails when it stops matching the table, so joining this class without being
// wired is a red rather than an omission.
//
// ⚠ COMMENT STRIPPING IS QUOTE-AWARE AND TAKES TRAILING COMMENTS TOO, which the siblings' does
// not. It has to: the MCP site already ends in a trailing `//` comment, so a stripper that only
// drops whole-line comments would let `mcp.New(…) // WithRateLimit(aiLimiter) removed` satisfy
// rule B — the documentation reported as the code. The vacuity check asserts its own premise in
// BOTH shapes (a whole-line sentinel and a trailing-comment sentinel must each be present in the
// raw bytes and absent after stripping); either half alone can only agree with itself.
//
// Controls: scripts/w31-llm-ratelimit-wiring-controls-2k9r.py.

// llmSurfaces is the pinned classification of every surface that takes a *ratelimit.Limiter
// through a WithRateLimit hook. `pkg` is matched against rule A's recomputed census; `ctor` is
// the constructor whose statement rule B requires the WithRateLimit call to be part of.
var llmSurfaces = []struct {
	pkg  string
	ctor string
	why  string
}{
	{
		pkg:  "ai",
		ctor: "ai.NewHandler(",
		why: "the five /v1/workspaces/{wsID}/ai/… routes call Lens directly. ai.Handler.limited " +
			"returns the UNWRAPPED handler when h.limit is nil, so without this call every AI " +
			"route is unmetered — no 429, no boot warning, no failing test.",
	},
	{
		pkg:  "mcp",
		ctor: "mcp.New(",
		why: "the ask_docs tool reaches Lens on Docs's single service key, and /mcp is an AGENT " +
			"door — the one caller that can drive it in a loop. callTool's ceiling is " +
			"`llmTools[name] && s.limit != nil && …`, so a nil limiter skips the check entirely: " +
			"MEASURED at 50/50 ask_docs calls served through a server built without this line, " +
			"against the 3rd-of-2 refusal internal/mcp/sec_ratelimit_test.go pins with it.",
	},
	{
		pkg:  "search",
		ctor: "search.NewHandler(",
		why: "GET /v1/workspaces/{wsID}/search embeds the query via Lens on its semantic half. " +
			"search.Handler.Mount mounts the route with NO middleware when h.limit is nil — the " +
			"route still answers, so the only visible difference is the bill.",
	},
}

// mainPath and internalRoot are relative to this package's directory.
const (
	mainPath     = "../../cmd/docs/main.go"
	internalRoot = ".."
)

// withRateLimitDecl matches a method declaration named WithRateLimit on any receiver. Rule A's
// population is the set of packages holding one.
var withRateLimitDecl = regexp.MustCompile(`func \([^)]*\) WithRateLimit\(`)

func TestRateLimit_MainWiresEveryLLMSpendCeiling(t *testing.T) {
	raw, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	statements := codeStatements(string(raw))
	body := strings.Join(statements, "\n")

	// ── vacuity: the stripper must actually strip, proved against phrases that ARE in the file,
	// one in a whole-line comment and one in a trailing comment. A stripper that handles only the
	// first shape is the hole rule B is exposed to (see the note above).
	for _, s := range []struct{ tag, sentinel string }{
		{"A-STRIP-LINE", "safer than silently unlimited"},
		{"A-STRIP-TRAILING", "far faster than a human clicks"},
	} {
		if !strings.Contains(string(raw), s.sentinel) {
			t.Fatalf("[%s-SENTINEL] the vacuity sentinel %q is no longer in %s at all, so its "+
				"absence below proves nothing about comment stripping — pick a phrase that IS in "+
				"a comment of that shape", s.tag, s.sentinel, mainPath)
		}
		if strings.Contains(body, s.sentinel) {
			t.Fatalf("[%s] comment stripping is not working for this comment shape — this guard "+
				"would pass on prose alone", s.tag)
		}
	}

	// ── rule A: the POPULATION, recomputed from the tree rather than restated.
	got := packagesDeclaringWithRateLimit(t)
	want := make([]string, 0, len(llmSurfaces))
	for _, s := range llmSurfaces {
		want = append(want, s.pkg)
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf(`[A-POPULATION] the set of packages declaring a WithRateLimit hook has changed.

  measured in internal/: %v
  classified below:      %v

Every package in that set is a surface that spends on Lens and takes a ceiling. If one has just
joined, WIRE IT in cmd/docs/main.go and add it to llmSurfaces with its constructor — otherwise
this guard keeps checking three sites while the fourth surface is unmetered, and it will say so
by staying green. If one has genuinely left, remove its entry here in the same change.`, got, want)
	}

	// ── rule B: each classified surface is actually wired, in code and not in prose.
	for _, s := range llmSurfaces {
		if !wiredInSameStatement(statements, s.ctor) {
			t.Errorf(`[B-WIRED %s] cmd/docs/main.go no longer calls WithRateLimit(...) on the value
%s...) returns.

%s

A nil limiter is UNTHROTTLED on all three surfaces, so deleting this call changes no status code,
logs nothing, and — measured at a0667e4 with all three removed — leaves gofmt, go vet, go build
and the whole real-Postgres go test -race suite green. That is why it is pinned here.

If the ceiling genuinely moved off the constructor's chain, update this entry to name its new
home. Do NOT satisfy this by naming the assigned variable: none of the three variable names
appears in main.go, because every site is a method chain (see the note above).`,
				s.pkg, s.ctor, s.why)
		}
	}
}

// wiredInSameStatement reports whether some statement holds both the constructor call and a
// WithRateLimit call — i.e. the limiter is attached to the value that constructor returns.
func wiredInSameStatement(statements []string, ctor string) bool {
	for _, st := range statements {
		if strings.Contains(st, ctor) && strings.Contains(st, "WithRateLimit(") {
			return true
		}
	}
	return false
}

// codeStatements returns main.go's lines with every comment removed, and with Go method-chain
// continuations spliced onto the line they continue. The splice is why rule B can be stated as
// "in the same statement" at all: main.go writes the MCP wiring as `mcp.New(…).` on one line and
// `WithRateLimit(aiLimiter)` on the next, and a line-at-a-time reader sees two unrelated
// fragments. The rule is narrow on purpose — a line is joined to the next ONLY when it ends in
// `.` — because a broader joiner (blocks, open parens) would eventually put a constructor and an
// unrelated WithRateLimit into one "statement" and make rule B satisfiable by coincidence.
func codeStatements(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		line = stripComment(line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if n := len(out); n > 0 && strings.HasSuffix(strings.TrimSpace(out[n-1]), ".") {
			out[n-1] += strings.TrimSpace(line)
			continue
		}
		out = append(out, line)
	}
	return out
}

// stripComment removes a `//` comment — whole-line or trailing — respecting string, rune and raw
// literals so a `//` inside a quoted value is not mistaken for one. Block comments are not
// handled: main.go contains none, and a stripper that silently mishandled one would be worse
// than one that plainly does not claim to.
func stripComment(line string) string {
	var inStr, inRune, inRaw bool
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inRaw:
			if c == '`' {
				inRaw = false
			}
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case inRune:
			if c == '\\' {
				i++
			} else if c == '\'' {
				inRune = false
			}
		case c == '`':
			inRaw = true
		case c == '"':
			inStr = true
		case c == '\'':
			inRune = true
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}

// packagesDeclaringWithRateLimit returns, sorted, the internal/ package directory names holding a
// WithRateLimit method declaration. Test files are excluded: a fixture declaring one is not a
// production surface, and counting it would red this guard for a change that spends nothing.
func packagesDeclaringWithRateLimit(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.WalkDir(internalRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		if withRateLimitDecl.Match(src) {
			seen[filepath.Base(filepath.Dir(path))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("[A-WALK] walking %s: %v", internalRoot, err)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
