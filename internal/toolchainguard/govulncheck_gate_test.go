package toolchainguard

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// govulncheck_gate_test.go — the supply-chain gate exists, and it is a JOB rather than a word.
//
// W6.33 measured this repo's actual state on clean main: `govulncheck ./...` reported TEN CALLED
// vulnerabilities — CALLED, not the separate "in modules you require but your code doesn't appear
// to call" bucket govulncheck counts on its own line — and ci.yaml contained the word govulncheck
// ZERO times. talyvor-lens had gated on govulncheck every build for the whole session; docs and
// track, the two repos that hold real Postgres, were not watched at all. The asymmetry was as much
// the finding as the ten were.
//
// ⚠ THIS FILE IS AN ORG-LEVEL CLASS GUARD AND IS DELIBERATELY THE SAME IN talyvor-track
// (internal/migrate/govulncheck_gate_test.go) — the reasoning scripts/check-compose-secrets.sh
// already records: two repos shipped the identical hole independently, so a per-repo instrument
// cannot catch the next repo. Only the numbers above and the package clause differ.
//
// ⚠ WHY A TEST AND NOT JUST THE YAML. The eleven were not introduced by one bad commit — they
// accumulated because NOTHING WOULD EVER SAY SO. That failure mode returns in full the moment the
// job is deleted or renamed, and deleting a job is a GREEN diff: every remaining check still
// passes, so nothing in CI objects to CI losing a check. This test is the thing that objects.
//
// ⚠⚠ AND IT IGNORES COMMENTS, DELIBERATELY — a substring check here would be the exact
// "documented but not wired" shape this queue keeps catching. The word "govulncheck" already
// appears in this repo's PROSE in several places: internal/toolchainguard/doc.go names it
// repeatedly, and so does ci.yaml's own job comment. A guard that
// grepped the raw file would therefore stay GREEN over a ci.yaml whose vuln job had been deleted
// and whose comment survived. Comment lines are stripped before anything is matched, and the
// match is on the INVOCATION (`govulncheck ./...`), not the name: `go install` alone is not a
// scan. The mutation controls for both of those are in TestTheGovulncheckGateGuardCanFail.
var govulncheckInvokeRe = regexp.MustCompile(`govulncheck["']?\s+\./\.\.\.`)

// jobHeaderRe matches a top-level job key in a GitHub workflow: exactly two spaces of indent,
// a name, a colon, nothing else.
var jobHeaderRe = regexp.MustCompile(`^  ([A-Za-z_][A-Za-z0-9_-]*):\s*$`)

// stripComment removes the prose from a line: a whole-line YAML comment, and a trailing
// comment introduced by " # ". Both forms are comments in YAML and — inside a `run:` block —
// in shell too, so one rule covers the file. ⚠ THE DIRECTION OF ITS ERRORS IS DELIBERATE: a
// literal " # " inside a genuine command would be truncated here and the gate reported MISSING,
// which is a false RED. A guard that fails loudly on an odd line is recoverable; one that passes
// on a comment is the defect this whole file exists to prevent.
func stripComment(line string) string {
	if strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
		return ""
	}
	if i := strings.Index(line, " # "); i >= 0 {
		return line[:i]
	}
	return line
}

// gateJob reports the ci.yaml job whose steps invoke govulncheck over the module, and the number
// of top-level jobs the parse saw. A zero job count means the parse is broken, not that the file
// is clean — the caller must distinguish those.
func gateJob(src string) (job string, jobsSeen int) {
	var current string
	for _, line := range strings.Split(src, "\n") {
		line = stripComment(line)
		if m := jobHeaderRe.FindStringSubmatch(line); m != nil {
			current, jobsSeen = m[1], jobsSeen+1
			continue
		}
		if job == "" && current != "" && govulncheckInvokeRe.MatchString(line) {
			job = current
		}
	}
	return job, jobsSeen
}

func ciYAMLSource(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yaml"))
	if err != nil {
		t.Fatalf("read ci.yaml: %v", err)
	}
	return string(b)
}

func TestCIGatesOnGovulncheck(t *testing.T) {
	job, jobsSeen := gateJob(ciYAMLSource(t))

	// ⚠ NON-VACUITY. A parse that finds no jobs at all satisfies "no job is missing a gate" and
	// satisfies nothing else. Report a broken instrument as broken rather than as a pass.
	if jobsSeen == 0 {
		t.Fatal("parsed ZERO top-level jobs out of ci.yaml — the parse is broken, and a broken " +
			"parse cannot see a missing gate either")
	}
	if job == "" {
		t.Fatalf("NO job in ci.yaml runs `govulncheck ./...` (%d job(s) parsed).\n"+
			"This repo carried TEN CALLED vulnerabilities precisely because no CI job would "+
			"ever say so (W6.33). If the gate is being removed on purpose, remove this test in "+
			"the same commit and say in the message what replaces it — do not leave a repo whose "+
			"only supply-chain signal is that someone remembers to run the tool by hand.", jobsSeen)
	}
	t.Logf("MEASURED: %d job(s) in ci.yaml; %q invokes govulncheck over the module.", jobsSeen, job)
}

// ⚠ THE POSITIVE CONTROL, IN THE FILE RATHER THAN IN A SESSION'S SCROLLBACK. Three sessions on
// this queue shipped guards that could not fail, and every one was caught only by a control like
// this. It runs the SAME function TestCIGatesOnGovulncheck runs, against mutated copies of the
// REAL ci.yaml, and asserts the guard goes red on each — including the mention-only mutation,
// which is the one a naive grep would pass.
func TestTheGovulncheckGateGuardCanFail(t *testing.T) {
	real := ciYAMLSource(t)
	if job, _ := gateJob(real); job == "" {
		t.Fatal("the unmutated ci.yaml already has no gate — TestCIGatesOnGovulncheck reports that; " +
			"this control cannot distinguish a working guard from a broken one until it is fixed")
	}

	// (1) The gate deleted outright: every line that invokes it removed.
	var kept []string
	for _, line := range strings.Split(real, "\n") {
		if govulncheckInvokeRe.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}
	if job, seen := gateJob(strings.Join(kept, "\n")); job != "" {
		t.Errorf("MUTANT SURVIVED: the invocation was deleted and the guard still found job %q "+
			"(%d jobs parsed) — it is not reading what it claims to read", job, seen)
	}

	// (2) The gate deleted but the PROSE left behind — the shape a grep would pass. Every
	// invocation becomes a comment mentioning govulncheck by name.
	var mentionOnly []string
	for _, line := range strings.Split(real, "\n") {
		if govulncheckInvokeRe.MatchString(line) {
			// ⚠ A WHOLE COMMENT LINE, NOT AN INLINE SPLICE. The first draft of this control
			// replaced the matched SUBSTRING with "# govulncheck ./...", which left the `#` in
			// the middle of a quoted shell path and re-inserted the very pattern under test —
			// the mutant "survived" because the mutation was wrong, and the run said so. A
			// mutation that does not produce the state it names tests nothing.
			mentionOnly = append(mentionOnly, "          # govulncheck ./... — removed, see PR")
			continue
		}
		mentionOnly = append(mentionOnly, line)
	}
	commented := strings.Join(mentionOnly, "\n")
	if !strings.Contains(commented, "govulncheck") {
		t.Fatal("the mention-only mutant does not mention govulncheck — the mutation is wrong, " +
			"not the guard")
	}
	if job, seen := gateJob(commented); job != "" {
		t.Errorf("MUTANT SURVIVED: the gate is gone and only a COMMENT names govulncheck, yet the "+
			"guard reported job %q (%d jobs parsed) — it is matching prose, which is exactly the "+
			"failure it exists to catch", job, seen)
	}

	// (4) The gate deleted and the mention left as a TRAILING comment on a surviving step —
	// the same false-green as (2) through the other comment syntax. This is what pins
	// stripComment's second rule; without it the guard reads prose after a live command.
	trailing := strings.Replace(strings.Join(kept, "\n"),
		"      - run: go vet ./...",
		"      - run: go vet ./... # govulncheck ./... used to run here",
		1)
	if !strings.Contains(trailing, "govulncheck ./...") {
		t.Fatal("the trailing-comment mutant does not carry the mention — the mutation is wrong, " +
			"not the guard")
	}
	if job, _ := gateJob(trailing); job != "" {
		t.Errorf("MUTANT SURVIVED: the gate is gone and govulncheck is named only in a TRAILING "+
			"comment, yet the guard reported job %q — it is matching prose", job)
	}

	// (3) The tool installed but never run — `go install` is not a scan.
	installOnly := govulncheckInvokeRe.ReplaceAllString(real, "govulncheck --help")
	if job, _ := gateJob(installOnly); job != "" {
		t.Errorf("MUTANT SURVIVED: no step scans the module and the guard still reported job %q", job)
	}
}
