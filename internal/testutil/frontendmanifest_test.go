package testutil

// frontendmanifest_test.go — THE WIRING THAT MAKES frontend/scripts/check-test-manifest.mjs A GATE,
// LOCKED FROM THE JOB THAT CANNOT BE EDITED AWAY WITH IT.
//
// ⚠⚠ THE FINDING THIS EXISTS FOR IS RECORDED IN THE SCRIPT'S OWN HEADER AND MEASURED AT `ce997ff`:
// one word, `describe(` -> `describe.skip(` on `frontend/src/components/SearchModal.cost.test.tsx`,
// disabled the six assertions that say a search row prints a document's TOTAL cost rather than one
// addend — and `npm test`, `npm run typecheck`, `npm run build`, `gofmt`, `go vet`, the real-Postgres
// `go test -race`, `check-compose-secrets.sh` and all three semgrep steps were EVERY ONE green.
// `internal/testutil/skipcensus_test.go` is this repository's class guard for exactly that failure
// and its walk names `frontend` among the directories it does not enter.
//
// ⚠⚠ WHY THE LOCK IS IN GO AND NOT IN THE FRONTEND. talyvor-suite shipped this same guard and its
// controls found that the closure is THE WIRING, NOT THE SCRIPT: with the script intact and the
// package.json entry reverted to a bare `vitest run`, the skip went uncaught and the chain was green
// exactly as it had been. A check cannot assert its own invocation — if the wiring is gone, the
// check does not run, and neither does anything it says. So the assertion lives in a DIFFERENT CI
// job (`test`) reached by a DIFFERENT command (`go test ./...`), where removing the frontend gate
// does not also remove the thing that notices.
//
// ⚠ IT PASSES ON ITS FIRST RUN BY CONSTRUCTION, WHICH IS THE PROPERTY TO DISTRUST — the same
// sentence shortflag_test.go carries, for the same reason. Its value is entirely in the mutations:
// scripts/w31-frontend-manifest-controls-b9d7.py drives them, and the one to read is the control
// that reverts `test` to `vitest run` with the skip still in place.
//
// ⚠ WHAT IT DOES NOT COVER, said plainly: an edit that removes the frontend gate AND this file in
// one change. Each closes the other against a single-file edit and neither closes a two-file one,
// and nothing in this repository could — the residual is a reviewable diff, which is where
// skipcensus_test.go's own residual was left too.

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifestFileFloor guards against this guard passing over an emptied manifest. The frontend held
// 28 test files when this was written; the floor sits well below that so ordinary deletions are
// reported by the checker's own file-set rule (with its reviewable diff) rather than by a panic
// here, and far above zero so a manifest reduced to nothing is a red.
const manifestFileFloor = 10

// npmTestInvocationFloor is the same shape as shortflag_test.go's workflowInvocationFloor and exists
// for the same reason: "the frontend gate is wired into `npm test`" says nothing if CI stopped
// running `npm test`. A ci.yaml with no frontend test step satisfies every other assertion here
// perfectly and means the opposite of what it reads.
const npmTestInvocationFloor = 1

func repoRootFor(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestFrontendTestScriptRunsTheManifestCheck(t *testing.T) {
	root := repoRootFor(t)
	path := filepath.Join(root, "frontend", "package.json")
	b, err := os.ReadFile(path)
	if err != nil {
		// READ-NOTHING IS NOT FIND-NOTHING.
		t.Fatalf("cannot read %s: %v — the guard read nothing, which is not the same as finding nothing", path, err)
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatalf("cannot parse frontend/package.json: %v", err)
	}
	script, ok := pkg.Scripts["test"]
	if !ok {
		t.Fatalf("frontend/package.json has no `test` script — ci.yaml's frontend job runs `npm test`, " +
			"so there is now no frontend suite at all, which is a larger finding than the one this guard was written for")
	}

	if !strings.Contains(script, "check-test-manifest.mjs") {
		t.Fatalf("frontend/package.json's `test` script does not run the manifest check:\n  test: %s\n\n"+
			"Without it `npm test` is a bare vitest run and a SKIPPED test is counted as a pass. MEASURED at "+
			"ce997ff: `describe.skip` on SearchModal.cost.test.tsx left `npm test` at EXIT 0 "+
			"(Tests 129 passed | 6 skipped (135)) with every other gate in the repository green. The check "+
			"must be IN this script rather than a separate CI step, because a separate step is one more "+
			"thing that can be dropped without anything noticing.", script)
	}

	// A CHAINED CHECK WHOSE EXIT CODE IS SWALLOWED IS NOT A GATE. `||` is the one-character way to
	// keep the invocation and lose the verdict, and it would leave every assertion above satisfied.
	if strings.Contains(script, "||") {
		t.Fatalf("frontend/package.json's `test` script contains `||`, which can swallow the manifest "+
			"check's non-zero exit:\n  test: %s\n\nThe check has to be able to fail the command CI runs.", script)
	}
}

func TestCIRunsTheFrontendSuite(t *testing.T) {
	root := repoRootFor(t)
	wf := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(wf)
	if err != nil {
		t.Fatalf("cannot read %s: %v — the guard read nothing, which is not the same as finding nothing", wf, err)
	}
	invocations := 0
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || (!strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml")) {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(wf, n))
		if rerr != nil {
			t.Fatalf("cannot read %s: %v", n, rerr)
		}
		// executedLines is shortflag_test.go's, deliberately shared rather than copied: what counts
		// as "a line a shell would actually run" — comments excluded, and a YAML `name:` value
		// excluded because it is prose that has already made one guard in this package unfailable —
		// is a question this repository should answer in exactly one place.
		for _, line := range executedLines(b) {
			if strings.Contains(line, "npm test") {
				invocations++
			}
		}
	}
	if invocations < npmTestInvocationFloor {
		t.Fatalf("found %d executed `npm test` invocation(s) in .github/workflows/, want at least %d. "+
			"The frontend gate is wired into that script; if CI does not run it, the wiring locked by "+
			"TestFrontendTestScriptRunsTheManifestCheck gates nothing.", invocations, npmTestInvocationFloor)
	}
}

// TestFrontendManifestCoversEveryTestFileOnDisk is the scope assertion, and it catches the one
// narrowing the checker is structurally blind to.
//
// ⚠ THE CHECKER COMPARES THE MANIFEST TO THE REPORT, AND BOTH ARE DOWNSTREAM OF vitest.config.ts's
// `include` GLOB. Narrow that glob and the excluded files leave the RUN and the report together; a
// single `npm run test:accept` then writes a manifest that agrees with the narrowed run, and the
// checker reports "ok" over a suite that stopped running part of itself. That is the allow-list
// shape this repository already shipped once in .semgrep (a rule narrowed to omit four packages,
// one of them carrying the live defect) and it is invisible from inside the pair it narrows.
//
// Disk is the third source neither of them can be narrowed against.
func TestFrontendManifestCoversEveryTestFileOnDisk(t *testing.T) {
	root := repoRootFor(t)
	b, err := os.ReadFile(filepath.Join(root, "frontend", "test-manifest.json"))
	if err != nil {
		t.Fatalf("cannot read frontend/test-manifest.json: %v — it is the committed record of what the "+
			"frontend suite is supposed to run and must be in the tree", err)
	}
	var manifest struct {
		Files map[string]int `json:"files"`
	}
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatalf("cannot parse frontend/test-manifest.json: %v", err)
	}

	onDisk := map[string]bool{}
	src := filepath.Join(root, "frontend", "src")
	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		n := d.Name()
		if !strings.HasSuffix(n, ".test.ts") && !strings.HasSuffix(n, ".test.tsx") {
			return nil
		}
		rel, rerr := filepath.Rel(filepath.Join(root, "frontend"), path)
		if rerr != nil {
			return rerr
		}
		onDisk[filepath.ToSlash(rel)] = true
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walking %s: %v", src, walkErr)
	}

	// THE VACUITY FLOOR, ASSERTED BEFORE THE COMPARISON. Two empty sets agree perfectly.
	if len(onDisk) < manifestFileFloor {
		t.Fatalf("[FLOOR] found only %d frontend test file(s) under %s, want at least %d — the walk is "+
			"mis-rooted or the suite was gutted, and an agreement between two near-empty sets means nothing",
			len(onDisk), src, manifestFileFloor)
	}
	if len(manifest.Files) < manifestFileFloor {
		t.Fatalf("[FLOOR] frontend/test-manifest.json lists only %d file(s), want at least %d — an emptied "+
			"manifest makes the checker's file-set rule report on nothing", len(manifest.Files), manifestFileFloor)
	}

	var missing, extra []string
	for f := range onDisk {
		if _, ok := manifest.Files[f]; !ok {
			missing = append(missing, f)
		}
	}
	for f := range manifest.Files {
		if !onDisk[f] {
			extra = append(extra, f)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("frontend/test-manifest.json disagrees with the test files on disk.\n"+
			"  on disk, not in the manifest (did vitest's `include` glob stop matching them?): %v\n"+
			"  in the manifest, not on disk (deleted without accepting?):                      %v\n\n"+
			"The checker compares the manifest to the vitest report and BOTH are downstream of\n"+
			"vitest.config.ts's `include`, so a narrowed glob plus one `npm run test:accept` makes the\n"+
			"pair agree about a suite that shrank. Disk is the source that cannot be narrowed with them.",
			missing, extra)
	}
}
