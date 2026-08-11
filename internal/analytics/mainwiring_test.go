package analytics

import (
	"os"
	"strings"
	"testing"
)

// THE TRIPWIRE ON THE ONE LINE THAT ARMS THE WORKSPACE ROLL-UP'S READ GATE.
//
// Every test in this package builds its own store, so cmd/docs/main.go's wiring is invisible to
// all of them — including privatespace_realpg_test.go, which proves the gate WORKS and would stay
// green with the production call site deleted. Two guards, deliberately blind to each other: that
// one can see a filter that decides wrongly and cannot see one that is never switched on.
//
// ⚠ WHAT IT CANNOT SEE, SAID PLAINLY. It pins a LITERAL, so it catches the call being deleted or
// renamed. It CANNOT see a wrong argument (an authorizer built over the wrong store), and it
// CANNOT see the store being constructed somewhere else and passed to the handler.
//
// ⚠ THE DELETED LINE IS NOT SILENT IN PRODUCTION, WHICH IS WHY THIS IS CHEAP RATHER THAN
// LOAD-BEARING: GetWorkspaceStats returns ErrNoPageReadGate, so the Analytics screen fails loudly
// rather than rendering an unfiltered roll-up or a zeroed one.
//
// ⚠ THE VACUITY CHECK ASSERTS ITS OWN PREMISE (the sibling in internal/search does not, and
// internal/freshness records why): the sentinel must be PRESENT in the raw bytes and ABSENT after
// comment stripping. Either half alone can only agree with itself.
func TestAnalytics_MainWiresThePageReadGate(t *testing.T) {
	const path = "../../cmd/docs/main.go"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var code []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	body := strings.Join(code, "\n")

	const sentinel = "fifth copy of the same seam"
	if !strings.Contains(string(raw), sentinel) {
		t.Fatalf("[A-SENTINEL] the vacuity sentinel %q is no longer in %s at all, so its absence "+
			"below proves nothing about comment stripping — pick a phrase that IS in the wiring "+
			"site's comment", sentinel, path)
	}
	if strings.Contains(body, sentinel) {
		t.Fatalf("[A-STRIP] comment stripping is not working — this guard would pass on prose alone")
	}

	if !strings.Contains(body, "analyticsStore.WithPageRead(") {
		t.Errorf(`[A-WIRED] cmd/docs/main.go no longer calls analyticsStore.WithPageRead(...).

GET /v1/workspaces/{wsID}/analytics/pages — the SPA's Analytics screen — authorized the WORKSPACE
and nothing else. Without this call the roll-up refuses with ErrNoPageReadGate; it will not report
an unfiltered answer, and it will not report a silently empty one. Before the gate existed it
reported the unfiltered roll-up: a workspace member with no grant on a private space received that
space's page TITLES, their page ids (a working /spaces/{space_id}/pages/{page_id} link) and their
view counts, while the by-page analytics route 403'd that same caller for that same page. Measured
in internal/analytics/privatespace_realpg_test.go.

If the gate genuinely moved somewhere else, update this literal to name its new home.`)
	}
}
