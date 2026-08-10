package ai

import (
	"os"
	"strings"
	"testing"
)

// THE TRIPWIRE ON THE ONE LINE THAT ARMS /ask's READ GATE.
//
// Every test in this package builds its own Handler, so cmd/docs/main.go's wiring is invisible to
// all of them — including privatespace_realpg_test.go, which proves the gate WORKS and would stay
// green with the production call site deleted.
//
// ⚠ THIS IS THE SECOND GUARD OF A DELIBERATELY BLIND PAIR, AND THE PAIRING IS THE POINT.
// privatespace_realpg_test.go can see a filter that decides wrongly and cannot see one that is
// never switched on; this can see the switch and knows nothing about what it does. Neither
// subsumes the other.
//
// ⚠ AND WHAT IT CANNOT SEE, SAID PLAINLY. It pins a LITERAL, so it catches the call being deleted
// or renamed — the failure a wiring convention actually has. It CANNOT see a wrong argument (an
// authorizer built over the wrong store), and it CANNOT see the route mounted on a Handler
// constructed somewhere else.
//
// ⚠ THE DELETED LINE IS NOT SILENT IN PRODUCTION, WHICH IS WHY THIS GUARD IS CHEAP RATHER THAN
// LOAD-BEARING: Handler.Ask refuses with a 500 and an ERROR log when h.access is nil, deliberately
// unlike internal/search's nil-means-unfiltered. This turns "the operator finds out from a support
// ticket" into "CI finds out".
//
// Comment lines are stripped before matching, because the wiring site carries a comment naming the
// call — a guard that cannot tell a mention from a call reports the documentation as the code.
func TestAsk_MainWiresThePageReadGate(t *testing.T) {
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

	// The comment-stripping is itself asserted, so a refactor that drops it turns this guard into
	// one a comment can satisfy — the exact way it would go quietly vacuous.
	//
	// ⚠ AND THE SENTINEL'S OWN PREMISE IS ASSERTED FIRST. A sentinel that is not in the file is
	// absent from the stripped body for the wrong reason, and the vacuity check would then itself
	// be vacuous — a guard-of-a-guard that can only ever agree. Both halves are needed: present in
	// the raw bytes, absent after stripping.
	const sentinel = "WHICH ANSWERS IN PROSE"
	if !strings.Contains(string(raw), sentinel) {
		t.Fatalf("[B-SENTINEL] the vacuity sentinel %q is no longer in %s at all, so its absence below proves "+
			"nothing about comment stripping — pick a phrase that IS in the wiring site's comment",
			sentinel, path)
	}
	if strings.Contains(body, sentinel) {
		t.Fatalf("[B-STRIP] comment stripping is not working — this guard would pass on prose alone")
	}

	if !strings.Contains(body, "aiHandler.WithPageRead(") {
		t.Errorf(`[B-WIRED] cmd/docs/main.go no longer calls aiHandler.WithPageRead(...).

POST /v1/workspaces/{wsID}/ai/ask authorizes the WORKSPACE in its own handler and nothing else.
Without this call the route refuses every request with a 500 — it will not answer from an
unfiltered corpus, and it will not answer from an empty one either. Before the gate existed it
answered from the unfiltered one: a workspace member with no grant on a private space had that
space's documents pasted into the prompt sent to Lens and cited back to them with deep links.
Measured in internal/ai/privatespace_realpg_test.go.

If the gate genuinely moved somewhere else, update this literal to name its new home.`)
	}
}
