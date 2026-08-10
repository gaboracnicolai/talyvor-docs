package search

import (
	"os"
	"strings"
	"testing"
)

// THE TRIPWIRE ON THE ONE LINE THAT ARMS THE GATE.
//
// Handler.access is nil-means-unfiltered, matching the convention Handler.limit already uses so the
// bare-handler tests in this package need no database. The cost of that convention is that the
// whole private-space fix lives or dies on ONE line in cmd/docs/main.go, and deleting it is silent:
// every test in this package still passes, because they all construct their own handler.
//
// ⚠ WHAT THIS GUARD IS AND IS NOT. It pins a LITERAL against the deployed main, so it catches the
// line being deleted or renamed — the failure mode a wiring convention actually has. It CANNOT see
// a wrong argument (a WithAccess given an authorizer with no page-meta looker fails closed, which
// is safe; one built over the wrong store would not be), and it CANNOT see the route being mounted
// on a handler built somewhere else. privatespace_realpg_test.go is what proves the gate WORKS;
// this only proves it is switched on in production.
//
// Comment lines are stripped before matching, because the wiring site carries a comment naming the
// call and a guard that cannot tell a mention from a call reports the documentation as the code.
func TestSearch_MainWiresTheAccessGate(t *testing.T) {
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

	// The comment-stripping is itself asserted, so a future refactor that drops it turns this
	// guard into one that a comment can satisfy — the exact way it would go quietly vacuous.
	if strings.Contains(body, "the tripwire") {
		t.Fatalf("comment stripping is not working — this guard would pass on prose alone")
	}

	if !strings.Contains(body, "searchHandler.WithAccess(") {
		t.Errorf(`cmd/docs/main.go no longer calls searchHandler.WithAccess(...).

The search route authorizes the WORKSPACE in its own handler and nothing else; the per-page read
gate is that call. Without it a workspace member with no grant on a PRIVATE space gets that space's
page titles, its name, and a ts_headline excerpt of the body back from
GET /v1/workspaces/{wsID}/search. Measured in internal/search/privatespace_realpg_test.go.

If the gate genuinely moved somewhere else, update this literal to name its new home.`)
	}
}
