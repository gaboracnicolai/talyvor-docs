package testutil_test

// A TEST THAT SKIPS IS A TEST THAT CANNOT FAIL THE BUILD, AND THIS REPOSITORY HAD ONE — INSIDE
// A SECURITY ASSERTION.
//
// .github/workflows/ci.yaml removed its "assert real-PG tests actually ran" step and wrote down,
// honestly, the one case that removal left uncovered:
//
//	WHAT IS NO LONGER GUARDED, stated plainly: an explicitly authored `t.Skip` inside a
//	real-PG test. That is a visible line in a pull-request diff, unlike the invisible
//	env-var failure this step was built for — which is now impossible. If that residual
//	case ever wants covering, the shape is a WHOLE-SUITE assertion over `go test -json`,
//	never another name filter.
//
// The residual case was not hypothetical. `internal/analytics/sec_viewer_test.go` — the ONLY
// test in the repository that drives the HTTP POST /spaces/{s}/pages/{p}/view route — skipped
// itself whenever no page_views row appeared. MEASURED at 08e0958: deleting one field from the
// handler's PageView literal (`Duration: in.Duration` → 0) stops every view in the product from
// being recorded while still answering 200, and under that mutation the FULL repo suite came
// back green, exit 0, zero FAILs, with that test reporting SKIP. The guard against readership
// forgery disarmed itself at exactly the moment the readership pipeline died.
//
// THIS CENSUS IS THE CLASS GUARD FOR THAT INSTANCE. It is deliberately NOT a name filter — the
// step CI deleted selected tests by name and covered 36 of 78, which is the failure mode that
// made a passing step mean less than it looked. This walks EVERY `*_test.go` in the tree, so
// its population is the whole suite by construction and there is no list to keep current.
//
// ⚠ SOURCE-DERIVED, SO IT CARRIES A FLOOR — the same reason viewbump_one_owner_test.go carries
// one. A regex census cannot notice the tree SHRINKING: delete every test file and a
// "no skips found" check reports clean and green. testFileFloor asserts the walk actually
// reached a plausible number of test files, so a vacuous pass is a red.
//
// ⚠ IT EXCLUDES ITSELF BY NAME, and that exclusion is the one hole in its own population. This
// file necessarily contains the literals it searches for. Excluding by filename rather than by
// some cleverer heuristic keeps the exclusion visible and exactly one file wide.
//
// ⚠ WHAT IT DOES NOT COVER, said rather than implied: a test that is vacuous WITHOUT skipping —
// one whose assertions are unreachable, or that asserts nothing. Nothing here can see that.
// This closes one named class: the test that reports SKIP and is counted as passing.
//
// IF A SKIP IS EVER GENUINELY WARRANTED, add it to allowedSkips with the reason. Making that an
// explicit, reviewed edit is the entire point: the failure this replaces was a skip nobody had
// to look at again after the day it was written.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testFileFloor guards against the census passing because it found nothing to read. The tree
// held 183 `*_test.go` files when this was written; the floor sits well below that so ordinary
// deletions do not trip it, and far above zero so an empty or mis-rooted walk does.
const testFileFloor = 120

// allowedSkips maps a repo-relative path to the reason its skip is intentional. Empty today,
// and that is the assertion: nothing in this repository currently turns itself off.
var allowedSkips = map[string]string{}

// skipCalls are the ways a Go test declares itself not-run. `t.Skipped()` is deliberately absent
// — it REPORTS status and does not cause a skip.
var skipCalls = []string{"t.Skip(", "t.Skipf(", "t.SkipNow(", "b.Skip(", "b.Skipf(", "b.SkipNow("}

func TestNoTestSkipsItselfOffTheBuild(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// The census file itself holds every literal it looks for.
	const self = "internal/testutil/skipcensus_test.go"

	scanned := 0
	offenders := map[string][]string{}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "frontend", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == self {
			return nil
		}
		scanned++
		if _, ok := allowedSkips[rel]; ok {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, call := range skipCalls {
				if strings.Contains(line, call) {
					offenders[rel] = append(offenders[rel],
						strings.TrimSpace(line)+"  (line "+itoa(i+1)+")")
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// THE FLOOR. Asserted before the verdict, so "found no skips" can never be reported by a
	// walk that found no tests.
	if scanned < testFileFloor {
		t.Fatalf("[FLOOR] census scanned only %d test files under %s, want >= %d — "+
			"the walk is mis-rooted or the tree shrank, and a clean result from it means nothing",
			scanned, root, testFileFloor)
	}
	t.Logf("[FLOOR] census read %d test files", scanned)

	if len(offenders) > 0 {
		for file, lines := range offenders {
			for _, l := range lines {
				t.Errorf("[NO-SKIP] %s: %s", file, l)
			}
		}
		t.Errorf("[NO-SKIP] a skipping test is counted as passing: `go test ./...` stays green " +
			"and CI has no step that reads skip status. If the skip is intentional, add the file " +
			"to allowedSkips with the reason; if it guards a precondition, make it a failure.")
	}
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
