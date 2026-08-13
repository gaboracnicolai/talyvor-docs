package export

import (
	"os"
	"strings"
	"testing"
)

// THE TRIPWIRE ON THE ONE LINE THAT ARMS THE CHILD EXPANSION'S READ GATE.
//
// Every test in this package builds its own exporter, so cmd/docs/main.go's wiring is invisible to
// all of them — including privatespace_realpg_test.go, which proves the gate WORKS and would stay
// green with the production call site deleted. Two guards, deliberately blind to each other: that
// one can see a gate that decides wrongly and cannot see one that is never switched on.
//
// ⚠ WHAT IT CANNOT SEE, SAID PLAINLY. It pins a LITERAL, so it catches the call being deleted or
// renamed. It CANNOT see a wrong argument (an authorizer built over the wrong store or a different
// looker), and it CANNOT see the exporter being constructed somewhere else and passed to the
// handler.
//
// ⚠ THE DELETED LINE IS NOT SILENT IN PRODUCTION, WHICH IS WHY THIS IS CHEAP RATHER THAN
// LOAD-BEARING: gatherPages returns ErrNoPageReadGate, so a children-including export 500s rather
// than shipping an unfiltered download or one quietly missing its children.
//
// ⚠ THE VACUITY CHECK ASSERTS ITS OWN PREMISE — the sentinel must be PRESENT in the raw bytes and
// ABSENT after comment stripping. Either half alone can only agree with itself.
func TestExport_MainWiresThePageReadGate(t *testing.T) {
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

	const sentinel = "THE SIXTH COPY OF THE SAME SEAM"
	if !strings.Contains(string(raw), sentinel) {
		t.Fatalf("[E-SENTINEL] the vacuity sentinel %q is no longer in %s at all, so its absence "+
			"below proves nothing about comment stripping — pick a phrase that IS in the wiring "+
			"site's comment", sentinel, path)
	}
	if strings.Contains(body, sentinel) {
		t.Fatalf("[E-STRIP] comment stripping is not working — this guard would pass on prose alone")
	}

	if !strings.Contains(body, "exporter.WithPageRead(") {
		t.Errorf(`[E-WIRED] cmd/docs/main.go no longer calls exporter.WithPageRead(...).

GET /v1/spaces/{spaceID}/pages/{pageID}/export?include_children=true appends OTHER pages' whole
documents to a response authorized for ONE page id. The route enforcer resolves {pageID} and can
say nothing about the children. Without this call a children-including export returns
ErrNoPageReadGate — it will not ship an unfiltered download, and it will not silently drop the
children either. Before the gate existed it shipped them: a member holding a page-level view grant
in a PRIVATE space received a sibling page's TITLE and FULL RENDERED BODY, in all four formats,
while the by-page route 403'd that same caller for that same page. Measured in
internal/export/privatespace_realpg_test.go.

If the gate genuinely moved somewhere else, update this literal to name its new home.`)
	}
}
