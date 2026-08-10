package permission_test

// NO HANDLER MAY DESCRIBE A MISSING ENFORCER AS "unguarded" — IT IS FAIL-CLOSED, AND SAYING
// OTHERWISE IS A FALSE STATEMENT ABOUT AN AUTHZ DEFAULT.
//
// Enforcer.Require on a nil receiver denies with 404 and never reaches the handler
// (middleware.go:174-181; TestEnforcer_NilReceiver_FailsClosed in this package is the
// behavioural guard). It USED to be pass-through — that was the defect, and the fix landed.
// The comments did not follow it: fifteen handlers went on saying "Without it the routes mount
// unguarded (tests)", describing the PRE-FIX behaviour, and exactly ONE of them
// (internal/database) had been corrected with an "A nil enforcer FAILS CLOSED (404)" addendum.
// Fourteen still described the hazard the fix removed.
//
// WHY THIS IS WORTH A TEST AND NOT JUST AN EDIT. The false version reads as PERMISSION: a
// reader deciding whether it is safe to omit WithAccess in a test concludes the request will
// reach the handler. It will not — it gets a 404. That is not a theoretical misreading: it
// happened while measuring W3.1 finding (11), where a chain built without WithAccess answered
// 404 to EVERY case, and only a positive control kept a vacuous result from being written down
// as a measurement.
//
// ⚠ THE PREDICATE IS NARROW ON PURPOSE. It reads only the doc comment attached to a
// `func (h *Handler) WithAccess(` whose parameters mention an Enforcer. A blanket ban on the
// word would be wrong: internal/pagelock/handler.go uses "an unguarded mount, which fails
// closed" and "an unguarded mount yields no level in context" to name the CONCEPT correctly,
// and a guard that reddened on those would be pressure to delete accurate prose.
//
// ⚠ AND IT IS A SOURCE-DERIVED GUARD, SO IT CARRIES A FLOOR. Parsing the tree for a phrase
// cannot notice the tree SHRINKING — delete every handler and a phrase census reports clean.
// wantAtLeast pins the population measured when this was written, so "found nothing, therefore
// nothing is wrong" fails instead of passing.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Measured at the commit that introduced this test: 15 of the 18 `WithAccess` methods under
// internal/ take a *permission.Enforcer (collab takes a SessionResolver, importer and
// templatelib take neither). A new handler pushes this up and is still checked; a DROP means
// the census stopped seeing files it used to see, and that is a failure, not a pass.
const wantAtLeast = 15

func TestHandlerDocs_NeverCallAMissingEnforcerUnguarded(t *testing.T) {
	root := filepath.Join("..", "..")

	var checked []string
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if name := info.Name(); name == "vendor" || name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Name() != "handler.go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(src), "\n")
		for i, ln := range lines {
			if !strings.HasPrefix(ln, "func (h *Handler) WithAccess(") {
				continue
			}
			if !strings.Contains(ln, "Enforcer") {
				continue // collab / importer / templatelib — not this class
			}
			rel, _ := filepath.Rel(root, path)
			checked = append(checked, rel)

			// The contiguous // block immediately above the func is its doc comment.
			for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "//"); j-- {
				if strings.Contains(lines[j], "unguarded") {
					offenders = append(offenders, rel+":"+strconv.Itoa(j+1)+": "+strings.TrimSpace(lines[j]))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// THE FLOOR, ASSERTED BEFORE THE VERDICT. A clean result from a census that read nothing is
	// the failure mode this whole class of guard has; check the instrument before the finding.
	if len(checked) < wantAtLeast {
		t.Fatalf("census found only %d enforcer-taking WithAccess doc comments, expected at least %d — "+
			"the guard is reading less of the tree than it did when it was written, so its "+
			"clean verdict means nothing. Found: %v", len(checked), wantAtLeast, checked)
	}

	if len(offenders) > 0 {
		t.Fatalf("%d handler doc comment(s) describe a missing Enforcer as \"unguarded\". It is "+
			"FAIL-CLOSED: Enforcer.Require on a nil receiver returns 404 and never reaches the "+
			"handler (see TestEnforcer_NilReceiver_FailsClosed). Say that instead:\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
