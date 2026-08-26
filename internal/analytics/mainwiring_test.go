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

	// ⚠ THE SPACE ROLL-UP'S GATE, AND IT FAILS THE OTHER WAY FROM THE ONE ABOVE — WHICH IS WHY IT
	// NEEDS ITS OWN LITERAL RATHER THAN TRUSTING THE ONE NEXT TO IT.
	//
	// A missing `WithPageRead` is LOUD in production: GetWorkspaceStats returns ErrNoPageReadGate
	// and the screen fails. A missing `WithSpaceAccess` is SILENT: `Enforcer.Require` on a nil
	// receiver denies with 404 (permission.TestEnforcer_NilReceiver_FailsClosed), so the space
	// roll-up route would answer "not found" for every space, forever, and look like a routing
	// typo rather than a dropped wiring. Fail-closed is the right default and it is exactly what
	// makes the omission hard to see — the feature is shipped, the tests in this package build
	// their own routers and stay green, and the only symptom is a screen section that never has
	// anything in it.
	if !strings.Contains(body, "analyticsHandler.WithSpaceAccess(") {
		t.Errorf(`[A-SPACE-WIRED] cmd/docs/main.go no longer calls analyticsHandler.WithSpaceAccess(...).

GET /v1/spaces/{spaceID}/analytics/pages is the SPACE roll-up — the third of the three scopes
talyvor.higgsfield.app/products/docs sells ("PAGE, SPACE AND ORG ROLLUPS"). Its route is mounted
unconditionally by analytics.Handler.Mount; without this call the enforcer is nil, Require denies,
and every space's roll-up is a 404. That is fail-closed and it is also invisible: nothing in this
package's own routers depends on main.go, and the SPA renders an empty section rather than an
error. internal/analytics/spacerollup_realpg_test.go proves the route WORKS when wired, and would
stay green with this line deleted.

If the gate genuinely moved somewhere else, update this literal to name its new home.`)
	}
}
