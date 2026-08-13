package testutil

// shortflag_test.go — THE SENTENCE "CI NEVER PASSES -short, SO CI CAN NEVER SKIP", MADE CHECKABLE.
//
// ⚠⚠ THE CLAIM IT LOCKS IS WRITTEN DOWN IN THIS PACKAGE AND WAS ENFORCED BY NOTHING.
// harness.go's requireDatabaseURL header reads:
//
//	"-short IS THE ESCAPE HATCH, and it must be explicit. `go test -short ./...` still skips, so
//	 a unit-only local run works exactly as before; CI never passes -short, so CI can never skip."
//
// The first two clauses are about this file's own code. The third is a claim about
// `.github/workflows/`, and `grep -c short .github/workflows/ci.yaml` returned **0** — not "no
// -short in the test step", but no instrument of any kind. The escape hatch that makes
// requireDatabaseURL's FAIL-don't-skip discipline usable locally is one word away from making the
// whole gate vacuous, and the word only has to be typed in a file this repository never reads.
//
// ⚠ MEASURED, NOT REASONED, BEFORE THIS GUARD WAS WRITTEN. With DOCS_TEST_DATABASE_URL unset:
//
//	go test -short -count=1 ./...   ->  exit 0, 244 tests SKIPPED, 0 failed, 0 packages failed
//	go test        -count=1 ./...   ->  exit 1   (the same tree, one flag apart)
//
// 244, against the 78 harness.go's header still quotes — the population that one flag disposes of
// has TRIPLED since the sentence was written, which makes the claim more load-bearing, not less.
// Every tenancy, IDOR, cross-workspace and money test in this repository is inside that 244.
//
// ⚠ THE SIBLING REPO HAS THE GUARD AND THIS ONE COPIED THE MECHANISM WITHOUT IT. harness.go names
// talyvor-track as the source of the fail-don't-skip shape; talyvor-track's ci.yaml also carries a
// step whose whole job is to assert CI never runs `go test` with -short. Docs took the hatch and
// left the lock. This is that lock, in the form this repository already uses for the neighbouring
// class (skipcensus_test.go) — a Go test rather than a CI step, which has one property a grep step
// does not: it also runs, and fails, in the very run that passed -short.
//
// ⚠⚠ THIS GUARD PASSES ON ITS FIRST RUN, BY CONSTRUCTION, AND THAT IS THE THING TO DISTRUST. It
// locks a property that holds today; nothing about today's tree can make it red. Its entire value
// is in the mutations, which are real edits to real files rather than arguments —
// scripts/w31-shortflag-controls-6b4d.py drives all five, including the two that catch a guard
// which is green for the wrong reason: a COMMENT that mentions -short must NOT red it (three such
// comment lines already exist in ci.yaml, one of them containing a whole `go test` command), and a
// ci.yaml with NO `go test` at all MUST red it — "nothing runs the suite" is not "nothing runs the
// suite with -short".

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflowInvocationFloor guards against this test passing because it read nothing.
//
// ⚠ IT IS COUNTED OVER THE WORKFLOWS ALONE, NOT OVER THE UNION WITH THE MAKEFILE, AND THE FIRST
// VERSION OF THIS GUARD GOT THAT WRONG. A floor of one over both sources is satisfied by the
// Makefile by itself, so deleting CI's `go test` step — the exact way to stop running the suite
// while every check stays green — scored NOT CAUGHT under control C3. The claim being locked is
// about CI, so the floor has to be about CI.
const workflowInvocationFloor = 1

// scanned sources: the CI definition (the subject of harness.go's claim) and the Makefile (the
// documented local command, and the obvious thing a future CI step would call instead of
// repeating the flags). A `-short` in either turns the same 244 tests off.
func shortFlagSources(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	wf := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(wf)
	if err != nil {
		// READ-NOTHING IS NOT FIND-NOTHING. A renamed or moved workflow directory would
		// otherwise make this guard green forever, which is the exact failure it exists to catch
		// one level up.
		t.Fatalf("cannot read %s: %v — the guard read nothing, which is not the same as finding "+
			"nothing", wf, err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || (!strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml")) {
			continue
		}
		out = append(out, filepath.Join(wf, n))
	}
	if len(out) == 0 {
		t.Fatalf("no workflow files under %s — CI is defined somewhere this guard does not look", wf)
	}
	mk := filepath.Join(root, "Makefile")
	if _, err := os.Stat(mk); err == nil {
		out = append(out, mk)
	}
	return out
}

// executedLines returns the lines of a file that a shell would actually run, i.e. everything whose
// first non-space character is not `#`. Both YAML comments and shell comments inside a `run: |`
// block have that shape.
//
// ⚠ THIS EXCLUSION IS NOT TIDINESS AND IT IS NOT HYPOTHETICAL. ci.yaml carries three comment lines
// containing the words `go test`, one of them a complete command (`go test -v -run 'RealPG|SEC4|A3'`)
// quoted while explaining a guard that was REMOVED. A substring scan over the file would take
// documentation as its subject: it would be red on prose that changes no behaviour, and — worse for
// a guard — the obvious way to quiet it is to edit the comment, which teaches the next reader that
// this test is about wording.
// ⚠⚠ AND A YAML `name:` VALUE IS PROSE TOO — THAT ONE WAS FOUND BY A CONTROL, NOT BY READING.
// ci.yaml's step is titled `- name: go test (real Postgres — real-PG tests must RUN, not skip)`,
// which contains the words `go test` and executes nothing. Counting it made the floor below
// unfailable: control C3 deletes the step's `run:` line and the guard stayed GREEN, because the
// title it left behind was still being counted as an invocation. The same mistake had a second
// edge the control did not have to find: a title containing the word `-short` would have been
// reported as an offending command.
func executedLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.HasPrefix(strings.TrimPrefix(t, "- "), "name:") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// hasShortFlag reports whether a `go test` command line carries the -short flag. Matched as a
// whitespace-delimited token so `-shortcut`, a path containing "short", or `-timeout 300s` cannot
// be mistaken for it, and so both spellings Go accepts (`-short`, `--short`, `-test.short`) are
// caught rather than only the one somebody happened to type.
func hasShortFlag(line string) bool {
	for _, tok := range strings.Fields(line) {
		tok = strings.TrimSuffix(tok, "=true")
		tok = strings.TrimSuffix(tok, "=1")
		switch tok {
		case "-short", "--short", "-test.short", "--test.short":
			return true
		}
	}
	return false
}

func TestCINeverRunsTheSuiteInShortMode(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	workflowInvocations := 0
	var offenders []string
	for _, path := range shortFlagSources(t, root) {
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("cannot read %s: %v — the guard read nothing, which is not the same as "+
				"finding nothing", path, rerr)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		inWorkflows := strings.HasPrefix(rel, ".github/workflows/")
		for _, line := range executedLines(b) {
			if !strings.Contains(line, "go test") {
				continue
			}
			if inWorkflows {
				workflowInvocations++
			}
			if hasShortFlag(line) {
				offenders = append(offenders,
					fmt.Sprintf("%s: %s", rel, strings.TrimSpace(line)))
			}
		}
	}

	// THE VACUITY FLOOR, AND IT IS A DIFFERENT ASSERTION FROM THE ONE BELOW. A ci.yaml that stopped
	// running the suite at all would satisfy "no invocation carries -short" perfectly, and the
	// resulting green would mean the opposite of what it says.
	if workflowInvocations < workflowInvocationFloor {
		t.Fatalf("found %d executed `go test` invocation(s) in .github/workflows/, want at least "+
			"%d. Either CI no longer runs the suite at all — which is a larger finding than the one "+
			"this guard was written for — or the scan is reading the wrong tree.",
			workflowInvocations, workflowInvocationFloor)
	}

	if len(offenders) > 0 {
		t.Fatalf("`go test` is invoked with -short:\n  %s\n\n"+
			"-short is the LOCAL escape hatch from internal/testutil.requireDatabaseURL, and in CI "+
			"it is a silent bypass of the entire real-Postgres suite. MEASURED with "+
			"DOCS_TEST_DATABASE_URL unset: `go test -short ./...` exits 0 with 244 tests SKIPPED "+
			"and zero failures, while the same tree without the flag exits 1. Every tenancy, IDOR "+
			"and cross-workspace test in this repository is inside that 244, and a skipped IDOR "+
			"suite is indistinguishable from a passing one — which is the whole reason "+
			"requireDatabaseURL fails instead of skipping. If a unit-only CI job is ever genuinely "+
			"wanted, it must be a SEPARATE job that does not stand in for this one, and this guard "+
			"must be taught the difference deliberately.",
			strings.Join(offenders, "\n  "))
	}
}
