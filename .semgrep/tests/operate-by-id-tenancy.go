// Fixture for .semgrep/operate-by-id-tenancy.yml. NOT PRODUCT CODE — every violation below is
// deliberate, and `semgrep --test` asserts that each expect-a-finding annotation is flagged and
// each expect-silence annotation is not. (Those two tokens are deliberately not spelled here:
// the annotation parser reads THIS comment too, and a rule name quoted in prose fails the run
// with a mismatch — measured, first attempt.)
//
// WHY THIS FILE EXISTS. docs-no-url-param-workspace-scope narrowed silently once already: it
// carried a paths.include allow-list that omitted four packages, one of which was shipping the
// exact defect the rule exists to catch. The allow-list is gone, but the rule's own comment then
// promised "any NEW unauthorized read is flagged by default" — and that was true only of a
// LITERAL param name, which nothing here could have told you. A rule with no fixture cannot be
// red-first, so its narrowing is invisible until the defect it stopped catching ships.
//
// HOW IT RUNS. `semgrep --test` pairs a rule file with a fixture of the SAME BASENAME IN THE SAME
// DIRECTORY, so CI copies both into one temp directory and runs there (see .github/workflows/ci.yaml).
// It is not run in place: `--config .semgrep/` would then load the copied rules twice.
//
// WHY `tests/`. MEASURED, because the reason is not the obvious one: semgrep's default ignore
// excludes any path with a `tests/` component, so the deliberate violations below are invisible to
// the product scan. A HIDDEN directory does NOT have that property — `.semgrep/*.go` IS scanned,
// which is where this fixture first sat, and the product scan flagged it. If a future semgrep
// version drops that default, this file starts failing the product scan LOUDLY, with an obvious
// cause; it cannot fail quietly. Nothing else in this repo relies on it: `frontend/src/test` is
// the only other such directory and holds no Go.
//
// COVERAGE, STATED SO IT IS NOT OVERREAD: the three rules in operate-by-id-tenancy.yml. The four
// in body-supplied-authority.yml have NO fixture — they need cross-function shapes this file does
// not model, and a green run here says nothing about them.
package tests

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type pool interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// wsParam pins the boundary of the literal rule in the direction that is EASY TO GET WRONG.
// Semgrep constant-propagates within a file, so this is NOT an escape from the literal rule —
// asserted below rather than assumed, because the fix that added the indirect rule was first
// justified by a control that claimed a const WAS an escape. It was not; the control was wrong.
const wsParam = "wsID"

func literalWorkspaceRead(r *http.Request) string {
	// ruleid: docs-no-url-param-workspace-scope
	return chi.URLParam(r, "wsID")
}

func constWorkspaceRead(r *http.Request) string {
	// ruleid: docs-no-url-param-workspace-scope
	return chi.URLParam(r, wsParam)
}

// argWorkspaceRead is the shape the literal rule cannot see: the param NAME arrives as an
// argument, so constant propagation — which does not cross a function boundary — has nothing to
// resolve. ratelimit.WorkspaceLimit and permission's two enforcers are this shape in production.
func argWorkspaceRead(r *http.Request, param string) string {
	// ruleid: docs-no-indirect-url-param-scope
	return chi.URLParam(r, param)
}

// A literal that is not the workspace is not this class. Both rules must stay silent here, or
// every by-id handler in the tree grows a suppression that means nothing.
func literalResourceRead(r *http.Request) string {
	// ok: docs-no-url-param-workspace-scope
	// ok: docs-no-indirect-url-param-scope
	return chi.URLParam(r, "pageID")
}

func byIDWriteUnscoped(ctx context.Context, p pool, id string) error {
	// ruleid: docs-by-id-write-requires-workspace-scope
	return p.Exec(ctx, `UPDATE pages SET title = $2 WHERE id = $1`, id, "t")
}

func byIDWriteScoped(ctx context.Context, p pool, id string, ws []string) error {
	// ok: docs-by-id-write-requires-workspace-scope
	return p.Exec(ctx, `UPDATE pages SET title = $2 WHERE id = $1 AND workspace_id = ANY($3)`, id, "t", ws)
}

func sprintfWriteUnscoped(ctx context.Context, p pool, col, id string) error {
	// ruleid: docs-by-id-write-requires-workspace-scope-sprintf
	q := fmt.Sprintf(`UPDATE pages SET %s = $2 WHERE id = $1`, col)
	return p.Exec(ctx, q, id, "t")
}

func sprintfWriteScoped(ctx context.Context, p pool, col, id string, ws []string) error {
	// ok: docs-by-id-write-requires-workspace-scope-sprintf
	q := fmt.Sprintf(`UPDATE pages SET %s = $2 WHERE id = $1 AND workspace_id = ANY($3)`, col)
	return p.Exec(ctx, q, id, "t", ws)
}
