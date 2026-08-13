package freshness

import (
	"os"
	"strings"
	"testing"
)

// THE TRIPWIRE ON THE ONE LINE THAT ARMS THE STALE REPORT'S READ GATE.
//
// Every test in this package builds its own engine, so cmd/docs/main.go's wiring is invisible to
// all of them — including privatespace_realpg_test.go, which proves the gate WORKS and would stay
// green with the production call site deleted. Two guards, deliberately blind to each other:
// that one can see a filter that decides wrongly and cannot see one that is never switched on.
//
// ⚠ WHAT IT CANNOT SEE, SAID PLAINLY. It pins a LITERAL, so it catches the call being deleted or
// renamed. It CANNOT see a wrong argument (an authorizer built over the wrong store), and it
// CANNOT see the engine being constructed somewhere else and passed to the handler.
//
// ⚠ THE DELETED LINE IS NOT SILENT IN PRODUCTION, WHICH IS WHY THIS IS CHEAP RATHER THAN
// LOAD-BEARING: GetStaleReport returns ErrNoPageReadGate, so the stale screen and the MCP tool
// both fail loudly rather than reporting an empty workspace. This turns "an operator notices the
// sidebar says zero" into "CI says so".
//
// ⚠ AND THE VACUITY CHECK ASSERTS ITS OWN PREMISE, WHICH THE SIBLING GUARD THIS ONE IS MODELLED ON
// DOES NOT. internal/search/mainwiring_test.go greps the comment-stripped main.go for a sentinel
// to prove the stripping works and never checks that the phrase is in the file at all — MEASURED:
// reword it there and that guard stays GREEN, so its vacuity check can only agree. Both halves are
// needed: the sentinel must be PRESENT in the raw bytes and ABSENT after stripping.
func TestFreshness_MainWiresThePageReadGate(t *testing.T) {
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

	const sentinel = "WHICH TWO SURFACES SHARE"
	if !strings.Contains(string(raw), sentinel) {
		t.Fatalf("[D-SENTINEL] the vacuity sentinel %q is no longer in %s at all, so its absence "+
			"below proves nothing about comment stripping — pick a phrase that IS in the wiring "+
			"site's comment", sentinel, path)
	}
	if strings.Contains(body, sentinel) {
		t.Fatalf("[D-STRIP] comment stripping is not working — this guard would pass on prose alone")
	}

	// ⚠ THE DIGEST'S ENUMERATOR, SAME FILE, SAME COMMENT-STRIPPED BODY, DIFFERENT CALL. Its
	// absence is loud in production (SendStaleDigestAll returns ErrNoWorkspaceEnumerator rather
	// than digesting an empty world), so this is cheap rather than load-bearing — the same
	// posture as [D-WIRED] below. What it CANNOT see is the enumerator being built over the
	// wrong store; digestworkspaces_realpg_test.go holds the behaviour.
	//
	// The other half of that call site needs no tripwire at all: Start(ctx) takes no workspace
	// id, so the pinned `Start(ctx, cfg.DefaultWorkspaceID)` cannot come back without a compile
	// error. A guard is only worth writing where the compiler is silent.
	if !strings.Contains(body, "freshEngine.WithWorkspaces(") {
		t.Errorf(`[D-DIGEST-ENUM] cmd/docs/main.go no longer calls freshEngine.WithWorkspaces(...).

The 09:00 UTC stale digest covers every workspace Docs holds content for. It used to be handed one
pinned cfg.DefaultWorkspaceID, which defaults to the literal "default" — a string no Track-minted
workspace matches — so it logged stale_pages=0 every day while every tenant's pages aged. Without
this call the sweep refuses (ErrNoWorkspaceEnumerator) rather than returning to that zero.

If the enumerator genuinely moved somewhere else, update this literal to name its new home.`)
	}

	if !strings.Contains(body, "freshEngine.WithPageRead(") {
		t.Errorf(`[D-WIRED] cmd/docs/main.go no longer calls freshEngine.WithPageRead(...).

GET /v1/workspaces/{wsID}/freshness and the MCP get_stale_pages tool share one engine method that
authorizes the WORKSPACE and nothing else. Without this call both refuse with ErrNoPageReadGate —
they will not report an unfiltered list, and they will not report a silently empty one. Before the
gate existed they reported the unfiltered list: a workspace member with no grant on a private space
received its stale page titles and a working /spaces/{space_id}/pages/{page_id} link. Measured in
internal/freshness/privatespace_realpg_test.go.

If the gate genuinely moved somewhere else, update this literal to name its new home.`)
	}
}
