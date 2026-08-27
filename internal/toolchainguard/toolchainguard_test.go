package toolchainguard

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The floor. Raising it means re-measuring govulncheck and updating doc.go's numbers; lowering it
// means restoring nine reachable stdlib advisories.
const (
	floorMajor = 1
	floorMinor = 26
	floorPatch = 6
)

var (
	toolchainRe = regexp.MustCompile(`(?m)^toolchain go(\d+)\.(\d+)\.(\d+)$`)
	goVersionRe = regexp.MustCompile(`go-version:\s*"(\d+)\.(\d+)(\.\d+)?"`)
	advisoryRe  = regexp.MustCompile(`GO-\d{4}-\d+`)
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("root %s has no go.mod", root)
	}
	return root
}

func read(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, parts...)...))
	if err != nil {
		t.Fatalf("read %v: %v", parts, err)
	}
	return string(b)
}

func atLeastFloor(maj, min, pat int) bool {
	if maj != floorMajor {
		return maj > floorMajor
	}
	if min != floorMinor {
		return min > floorMinor
	}
	return pat >= floorPatch
}

func num(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("non-numeric version component %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ⚠ THE SHIPPED FLOOR. Removing the directive restores nine reachable stdlib advisories to local
// and Docker builds.
func TestGoModPinsTheToolchainFloor(t *testing.T) {
	m := toolchainRe.FindStringSubmatch(read(t, "go.mod"))
	if m == nil {
		t.Fatalf("go.mod has no `toolchain goX.Y.Z` directive.\n\n"+
			"    Without it, local and Docker builds use whatever Go is installed. Measured at "+
			"go1.26.3 that was 10 CALLED vulnerabilities; at go%d.%d.%d it is 1, and the one that "+
			"remains is a dependency, not the stdlib.", floorMajor, floorMinor, floorPatch)
	}
	if maj, min, pat := num(t, m[1]), num(t, m[2]), num(t, m[3]); !atLeastFloor(maj, min, pat) {
		t.Errorf("go.mod pins toolchain go%d.%d.%d; the security floor is go%d.%d.%d. Lowering it "+
			"restores GO-2026-6218/6090/6089/6088/5972/5039/5037/5026 and GO-2026-5856.",
			maj, min, pat, floorMajor, floorMinor, floorPatch)
	}
}

// ⚠ THE LOCKSTEP, AND THE ONLY REASON THIS PACKAGE EXISTS. go.mod's directive does not reach CI:
// setup-go exports GOTOOLCHAIN=local. talyvor-track shipped a pin that changed nothing in CI for
// exactly this reason (W6.34), so here the two numbers are compared rather than trusted.
func TestEveryCIGoVersionPinMeetsTheFloor(t *testing.T) {
	pins := goVersionRe.FindAllStringSubmatch(read(t, ".github", "workflows", "ci.yaml"), -1)
	if len(pins) == 0 {
		t.Fatal("ci.yaml declares no `go-version:` pin — either the workflow stopped installing Go " +
			"or this parse is broken, and a broken parse reports perfect lockstep")
	}
	for _, p := range pins {
		pat := 0
		if p[3] != "" {
			pat = num(t, strings.TrimPrefix(p[3], "."))
		}
		if maj, min := num(t, p[1]), num(t, p[2]); !atLeastFloor(maj, min, pat) {
			t.Errorf("ci.yaml pins go-version %q, below go%d.%d.%d.\n"+
				"    setup-go exports GOTOOLCHAIN=local, so this pin — not go.mod's directive — is "+
				"what the job runs. Below the floor, CI tests a runtime the release is not built "+
				"with, and the gofmt gate formats with a different toolchain's gofmt.",
				p[0], floorMajor, floorMinor, floorPatch)
		}
	}
	t.Logf("MEASURED: %d go-version pin(s) in ci.yaml, all >= go%d.%d.%d.",
		len(pins), floorMajor, floorMinor, floorPatch)
}

// ⚠ IS THE CONTROL INSTALLED, OR IS IT WORKING? Those are different questions, and in W6.34 only
// this one could tell them apart — every other assertion passed while the pin was inert.
func TestTheRunningToolchainMeetsTheFloor(t *testing.T) {
	v := strings.TrimPrefix(runtime.Version(), "go")
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		// ⚠ A FAILURE, NOT A SKIP, AND internal/testutil's skip census IS WHY. Its message: "a
		// skipping test is counted as passing: `go test ./...` stays green and CI has no step that
		// reads skip status." That applies with force here — a security floor that SKIPS when it
		// cannot parse the toolchain is a floor anyone can switch off by running an odd toolchain.
		// Unparseable is unverified, and unverified is not clear.
		t.Fatalf("runtime.Version() = %q has no patch component, so the go%d.%d.%d floor cannot be "+
			"verified for the toolchain actually running. A floor that cannot be checked is not a "+
			"floor.", runtime.Version(), floorMajor, floorMinor, floorPatch)
	}
	if maj, min, pat := num(t, parts[0]), num(t, parts[1]), num(t, parts[2]); !atLeastFloor(maj, min, pat) {
		t.Errorf("this test binary was built by go%s, below the go%d.%d.%d floor.\n"+
			"    If the floor is not in effect here it is not in effect in CI either, and the pin "+
			"is a comment rather than a control — which is exactly what talyvor-track shipped "+
			"before its CI refused.", v, floorMajor, floorMinor, floorPatch)
	}
}

// ⚠ A FLOOR WITH NO REASON IS A NUMBER SOMEBODY LOWERS TO FIX A BUILD. Counted, not merely
// present: track's first version of this check accepted any single "GO-2026-" string, so deleting
// the whole justifying list still passed.
func TestTheFloorsRationaleNamesTheAdvisories(t *testing.T) {
	src := read(t, "go.mod")
	idx := toolchainRe.FindStringIndex(src)
	if idx == nil {
		// Also a failure rather than a skip, for the same reason. TestGoModPinsTheToolchainFloor
		// reports the missing directive too; duplicate reporting is the cheaper mistake.
		t.Fatal("no `toolchain` directive in go.mod, so there is no floor for a rationale to justify")
	}
	above := src[:idx[0]]
	ids := map[string]bool{}
	for _, m := range advisoryRe.FindAllString(above, -1) {
		ids[m] = true
	}
	const wantIDs = 4
	if len(ids) < wantIDs {
		t.Errorf("the toolchain floor's rationale names %d distinct advisory id(s); it justifies a "+
			"floor that clears nine, so it must name at least %d — written out in full, because "+
			"GO-2026-{6218,6090,...} is not greppable and an advisory nobody can search for is one "+
			"nobody finds when it matters.", len(ids), wantIDs)
	}
	if !strings.Contains(above, "govulncheck") {
		t.Error("the rationale does not mention govulncheck — without it the next reader cannot tell " +
			"a security floor from a preference")
	}
}
