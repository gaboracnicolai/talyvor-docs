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

// ─── THE SQL LITERAL THAT IS NOT THE CALL'S ARGUMENT ────────────────────────────────────────
//
// The three shapes below are the by-id write with its SQL held in a NAME rather than written at
// the call. docs-by-id-write-requires-workspace-scope binds $SQL to that name, so its
// metavariable-regex reads an identifier and matches nothing; the Sprintf rule does not apply
// because there is no Sprintf. MEASURED at 09d8c2e on the CI-pinned semgrep 1.165.0, BEFORE the
// indirect-literal rule existed: all three were flagged by NOTHING in .semgrep/ — the whole
// shipped rule set returned zero findings over a file containing exactly these.
//
// ⚠ WHY THE RULE IS ANCHORED ON THE ASSIGNMENT AND NOT THE CALL. The same choice the Sprintf rule
// made, for the same reason: the SQL is the evidence, and a rule that has to follow it to a call
// site is a dataflow problem that grows a blind spot per plumbing shape. Anchoring on the literal
// means a fourth way of handing SQL to a pool is covered the day it is written.

// ruleid: docs-by-id-write-requires-workspace-scope-indirect-literal
const byIDWriteConstUnscoped = `DELETE FROM pages WHERE id = $1`

// The scoped twin. A rule that flags this one is a rule every store method must suppress, which
// is how a class guard becomes noise and then becomes ignored.
// ok: docs-by-id-write-requires-workspace-scope-indirect-literal
const byIDWriteConstScoped = `DELETE FROM pages WHERE id = $1 AND workspace_id = ANY($2)`

// The positional index is $2 here DELIBERATELY. The sibling rule shipped a `\$1`-anchored regex and
// a `$2`/`$3` by-id write slipped it (block.Update); this rule inherited the corrected `\$\d+` and
// this case is what stops it from being narrowed back one character at a time.
func byIDWriteVarUnscoped(ctx context.Context, p pool, id string) error {
	// ruleid: docs-by-id-write-requires-workspace-scope-indirect-literal
	q := `UPDATE pages SET title = $1 WHERE id = $2`
	return p.Exec(ctx, q, "t", id)
}

func byIDWriteVarScoped(ctx context.Context, p pool, id string, ws []string) error {
	// ok: docs-by-id-write-requires-workspace-scope-indirect-literal
	q := `UPDATE pages SET title = $2 WHERE id = $1 AND workspace_id = ANY($3)`
	return p.Exec(ctx, q, id, "t", ws)
}

// The concatenated form THIS REPO ALREADY WRITES — `SELECT ` + cols + ` FROM …` is the house
// style in page, comment and space. Assigned to a name it is invisible; handed straight to the
// call it is caught (measured both ways). That difference is not a property anybody chose.
func byIDWriteConcatUnscoped(ctx context.Context, p pool, id string) error {
	// ruleid: docs-by-id-write-requires-workspace-scope-indirect-literal
	q := `UPDATE pages SET title = $2 WHERE id = $1 RETURNING ` + fixtureCols
	return p.Exec(ctx, q, id, "t")
}

const fixtureCols = `id, title`

// ⚠ THE MEASURED LIMIT, WRITTEN AS A FIXTURE SO IT IS A FACT RATHER THAN A HOPE. A statement
// assembled so that NO SINGLE LITERAL holds both the write and its `WHERE id = $N` is invisible
// to a literal-anchored rule, and the expect-silence annotation below is that limit stated out
// loud. It is not "covered"; it is KNOWN. comment.Store.ListByPage builds a SELECT exactly this
// way today, so the shape is house style rather than hypothetical — the day one of those becomes
// an UPDATE, nothing here sees it.
func byIDWriteFragmentedUnscoped(ctx context.Context, p pool, id string) error {
	// ok: docs-by-id-write-requires-workspace-scope-indirect-literal
	q := `UPDATE pages SET title = $2`
	q += ` WHERE id = $1`
	return p.Exec(ctx, q, id, "t")
}
