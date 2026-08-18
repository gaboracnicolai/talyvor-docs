// Fixture for .semgrep/body-supplied-authority.yml. NOT PRODUCT CODE — every violation below is
// deliberate, and `semgrep --test` asserts that each expect-a-finding annotation is flagged and
// each expect-silence annotation is not. (Those two tokens are deliberately not spelled in prose
// anywhere in this file: the annotation parser reads comments, and a rule name quoted in a
// sentence fails the run with a mismatch — the sibling fixture records the same trap.)
//
// WHY THIS FILE EXISTS. Until it did, all four rules in body-supplied-authority.yml were on
// check-semgrep-rule-scope.py's declared-unfixtured list, and the reason recorded there for the
// inverted-fallback rule was "needs the if-empty-then-verified shape against each of the six
// verified-identity helpers its metavariable-regex names". Writing the cases measured something
// that reason had the wrong shape for, and it is the whole value of this file:
//
// ⚠⚠ THE INVERTED-FALLBACK RULE COULD ONLY EVER FIRE AGAINST THE TWO HELPERS THE CODEBASE HAD
// ALREADY BANNED. Its pattern is `if $IN.$FIELD == "" { $IN.$FIELD = $VERIFIED }` and its
// metavariable-regex names EIGHT helpers (not six). SIX of those eight — AuthorizedMember,
// ActorFromContext, WorkspaceFromContext, MemberIDForWorkspace, SingleMemberID, SingleWorkspace —
// return `(string, bool)`, so a call to one CANNOT stand in the right-hand side of a single-value
// assignment: it does not compile. The only two that can are ActorOrEmpty and WorkspaceOrEmpty,
// and those are exactly the two the sibling rule (docs-no-ambiguous-actor-helpers) rejects
// outright and that every call site in this repository has already been migrated off. So the rule
// was incapable of producing a finding its sibling did not already produce, while its message
// named the recommended resolvers as the fix.
//
// MEASURED, not reasoned from the signatures: the two probes below (invertedAgainstActorFromContext,
// invertedAgainstWorkspaceFromContext) are the SAME defect written the only way Go allows against
// a two-value resolver, and against the shipped rule set they were flagged by NOTHING —
// `semgrep --config .semgrep/` returned zero findings over a file containing exactly them, on a
// tree whose product scan was also zero. The `-two-value` rule exists because of that measurement
// and both probes are now expect-a-finding cases; delete it and they go quiet again.
//
// ⚠ THE TWO RULES SHARE ONE VOCABULARY ON PURPOSE — the same eight-name regex, byte for byte,
// rather than the two-value rule naming only the six it can reach. Two lists that mean "a
// verified identity" are two definitions that can drift, which is the failure this repository
// keeps finding; one list that is partly unreachable in each rule is not.
//
// HOW IT RUNS. `semgrep --test` pairs a rule file with a fixture of the SAME BASENAME IN THE SAME
// DIRECTORY, so CI copies both into one temp directory and runs there (see .github/workflows/ci.yaml).
//
// WHY `tests/`. Semgrep's default ignore excludes any path with a `tests/` component, so the
// deliberate violations below are invisible to the product scan — measured, and a HIDDEN
// directory does NOT have that property. The sibling fixture's header carries the full note.
//
// COVERAGE, STATED SO IT IS NOT OVERREAD: the five rules in body-supplied-authority.yml. What
// these cases CANNOT establish is what the rule file's own KNOWN LIMITS section already says —
// none of these are dataflow rules, and none of them see across a function boundary. A green run
// here says those limits are unchanged, not that they are gone.
package tests

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

// bodyIn stands in for model.Page / PageLink / ChangelogEntry — the real request structs that
// carry a tenancy and an attribution field, which is what makes this class possible at all.
type bodyIn struct {
	WorkspaceID string
	CreatedBy   string
	ViewerID    string
	Title       string
}

type fixtureStore struct{}

func (fixtureStore) Create(ctx context.Context, in bodyIn) error { return nil }

var fixtureSt fixtureStore

// ─── A. The cross-tenant WRITE shape ────────────────────────────────────────────────────────
//
// Decode the body, hand the whole struct to a store, never consult an approved resolver. This is
// page.Create as it shipped: the caller chose the tenant, and SEC-4's `workspace_id = ANY(verified
// set)` filter then surfaced the planted row to the VICTIM.

func createNoResolver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	// ruleid: docs-no-body-supplied-authority
	fixtureSt.Create(ctx, in)
}

// The sanitized twin. A rule that still flags this is a rule every create handler suppresses,
// which is how a class guard becomes noise and then becomes ignored.
func createWithResolver(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	ws, _ := permission.WorkspaceFromContext(ctx)
	in.WorkspaceID = ws
	// ok: docs-no-body-supplied-authority
	fixtureSt.Create(ctx, in)
}

// ─── B. The forged-identity FIELD shape ─────────────────────────────────────────────────────
//
// analytics.RecordView read viewer_id straight off the body and never assigned it, so the body
// forged who read a page — into COUNT(DISTINCT viewer_id), the figure the dashboard reports.

func recordViewForgedViewer(w http.ResponseWriter, r *http.Request) {
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	// ruleid: docs-no-body-supplied-authority-field
	_ = in.ViewerID
}

// Reading a CLAIMED workspace in order to authorize it is the reference implementation
// (space.Create), not the bug. Both A and B must stay silent here: the read is the thing that
// rejects a non-member, and the call is what sanitizes the handler.
func createAuthorizesClaimedWorkspace(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	// ok: docs-no-body-supplied-authority-field
	authz.AuthorizeWorkspace(ctx, in.WorkspaceID)
	// ok: docs-no-body-supplied-authority
	fixtureSt.Create(ctx, in)
}

// ─── C + D. The inverted fallback, single-value form ─────────────────────────────────────────
//
// changelog.Create shipped exactly this. It needs no precondition — it is unconditional forgery
// for every caller. Note that BOTH rules fire, on two different lines, and that is the point of
// the section below: on this shape the inverted-fallback rule adds nothing the helpers rule does
// not already say.

func invertedAgainstOrEmpty(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	// ruleid: docs-no-inverted-identity-fallback
	if in.CreatedBy == "" {
		// ruleid: docs-no-ambiguous-actor-helpers
		in.CreatedBy = authz.ActorOrEmpty(ctx)
	}
}

// A bare new use of a banned helper, with no fallback around it. The helpers rule is exact rather
// than an approximation, so this case is what stops it being narrowed to the fallback shape.
func bareAmbiguousHelper(ctx context.Context) string {
	// ruleid: docs-no-ambiguous-actor-helpers
	return authz.WorkspaceOrEmpty(ctx)
}

// ─── C'. The inverted fallback, two-value form ──────────────────────────────────────────────
//
// The same defect against the resolvers the rule messages actually recommend. Go forces the
// unpack, so the verified identity arrives as a LOCAL and the single-value rule's regex — which
// reads the right-hand side's own source text — has nothing to match. Both of these were silent
// against the whole shipped rule set; see this file's header for the measurement.

func invertedAgainstActorFromContext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	actor, _ := permission.ActorFromContext(ctx)
	// ruleid: docs-no-inverted-identity-fallback-two-value
	if in.CreatedBy == "" {
		in.CreatedBy = actor
	}
}

// The nested comma-ok, which is the shape a handler that checks `ok` actually writes. One rule
// covers both because its `pattern-inside` reaches the if-statement's init clause — measured, not
// assumed: this case is what says so.
func invertedAgainstWorkspaceFromContext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	// ruleid: docs-no-inverted-identity-fallback-two-value
	if in.WorkspaceID == "" {
		if ws, ok := permission.WorkspaceFromContext(ctx); ok {
			in.WorkspaceID = ws
		}
	}
}

// THE CORRECT SHAPE, and the reason the two-value rule is anchored on the emptiness test rather
// than on "a local from a resolver reaches a body field". The verified identity is not a default;
// it is the only answer, so it is assigned unconditionally. This must stay silent or every
// correctly-written handler in the tree grows a suppression that means nothing.
func verifiedAssignedUnconditionally(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var in bodyIn
	json.NewDecoder(r.Body).Decode(&in)
	actor, _ := permission.ActorFromContext(ctx)
	ws, _ := permission.WorkspaceFromContext(ctx)
	in.CreatedBy = actor
	in.WorkspaceID = ws
	// ok: docs-no-inverted-identity-fallback-two-value
	if in.Title == "" {
		in.Title = "Untitled"
	}
	// ok: docs-no-body-supplied-authority
	fixtureSt.Create(ctx, in)
}
