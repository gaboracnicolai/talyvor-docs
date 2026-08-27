// Package authz is SEC-4 Layer 1's membership layer, sitting on gatewayauth's transit-proof
// boundary. gatewayauth proved WHO the caller is (verified email in context); authz resolves
// that email to the workspaces the caller is a member of (via workspace_members) and puts the
// membership set in context. Docs's by-id routes are flat (/v1/spaces/{spaceID}/pages/{pageID}),
// not workspace-in-path like Track, so authz does NOT authorize a single path workspace — it
// exposes the caller's workspace SET, and Layer 2 scopes every by-id query to it
// (WHERE workspace_id = ANY(set)). The workspace in every store filter comes from the verified
// membership, never from the spoofable X-Member-Id / X-Talyvor-Workspace headers.
package authz

import (
	"context"
	"net/http"

	"github.com/talyvor/docs/internal/gatewayauth"
)

// Membership is one (workspace, member, role) the verified caller belongs to.
type Membership struct {
	WorkspaceID string
	MemberID    string
	Role        string
}

// Resolver resolves a gateway-verified email to its memberships. The PG impl queries
// workspace_members (resolver.go); tests inject a fake.
type Resolver interface {
	MembershipsByEmail(ctx context.Context, email string) ([]Membership, error)
}

type ctxKey struct{}

type authCtx struct {
	email       string
	memberships []Membership
	authWS      string // set by WithAuthorized after a per-call workspace authorization (MCP chokepoint)
	authMember  string
	// passed records that this context came through the BOUNDARY — i.e. that WithMemberships
	// installed it, which the middleware only does after gatewayauth verified an identity and the
	// resolver answered.
	//
	// ⚠ IT EXISTS BECAUSE THE PRESENCE OF AN authCtx WAS NOT THE SAME QUESTION, AND TWO DOCSTRINGS
	// SAID IT WAS. Memberships and Email both promised "ok=false when the request never passed the
	// boundary" and both derived that from whether the type assertion succeeded. WithAuthorized
	// installs an authCtx unconditionally — it is written to work on top of whatever is there — so
	// MEASURED on a bare context: WithAuthorized(context.Background(), "ws-x", "member-x") made
	// Memberships report ok=TRUE with an empty set and Email report ("", TRUE). Both are the exact
	// opposite of what they say.
	//
	// ⚠ IT WAS A TRAP AND NOT A LIVE DEFECT, MEASURED RATHER THAN ASSUMED, AND THAT IS WHY THE FIX
	// IS A FIELD AND NOT A REWRITE: WorkspaceIDs still returned the empty set (so every by-id query
	// still denied) and AuthorizeWorkspace still refused (its loop has nothing to match). The
	// production caller — internal/mcp's callTool — only reaches WithAuthorized after
	// AuthorizeWorkspace has already succeeded, which cannot happen without memberships. So
	// nothing shipped was wrong; what was wrong was that the one consumer who trusts that `ok`
	// (internal/collab's refreshRoster, which refuses on !ok || email == "") was protected by its
	// own second check rather than by the contract it was reading.
	passed bool
}

// Memberships returns the verified caller's full membership set. ok=false when the request
// never passed the boundary.
func Memberships(ctx context.Context) ([]Membership, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*authCtx)
	if !ok || !ac.passed {
		return nil, false
	}
	return ac.memberships, true
}

// Email returns the verified caller email. ok=false when the request never passed the boundary.
func Email(ctx context.Context) (string, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*authCtx)
	if !ok || !ac.passed {
		return "", false
	}
	return ac.email, true
}

// WorkspaceIDs returns the workspace ids the verified caller belongs to — the scope every
// by-id store query filters on (WHERE workspace_id = ANY(...)). An empty slice (caller has no
// memberships, or the request never passed the boundary) correctly denies all by-id access:
// ANY(empty) matches nothing → 404. This is the fail-closed property.
func WorkspaceIDs(ctx context.Context) []string {
	ms, _ := Memberships(ctx)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.WorkspaceID)
	}
	return out
}

// SingleWorkspace returns the caller's workspace WHEN they belong to exactly one.
//
// ⚠ DO NOT CALL THIS FROM A HANDLER. `.semgrep/body-supplied-authority.yml`'s
// docs-no-ambiguous-actor-helpers rejects `authz.SingleWorkspace(...)` outright, and this
// docstring used to end "the Docs common case (one workspace per instance) ... a caller that
// needs an unambiguous create-target must handle the ambiguous case explicitly" — an invitation
// to the exact shape seven packages in this repository each shipped and each had to fix.
// Discarding the ok reproduces WorkspaceOrEmpty inline; honouring it 403s every MULTI-WORKSPACE
// member on a route their membership entitles them to. Derive the workspace from the resource
// the route authorized (permission.WorkspaceFromContext) or authorize the claimed one
// (AuthorizeWorkspace) — both are correct for any membership count.
//
// It survives as WorkspaceOrEmpty's implementation and has no other caller in the tree.
func SingleWorkspace(ctx context.Context) (string, bool) {
	ms, ok := Memberships(ctx)
	if !ok || len(ms) != 1 {
		return "", false
	}
	return ms[0].WorkspaceID, true
}

// MemberIDForWorkspace returns the caller's member id in a specific workspace (the resolved
// actor for attribution). Replaces the spoofable X-Member-Id. ok=false if not a member.
func MemberIDForWorkspace(ctx context.Context, wsID string) (string, bool) {
	ms, ok := Memberships(ctx)
	if !ok {
		return "", false
	}
	for _, m := range ms {
		if m.WorkspaceID == wsID {
			return m.MemberID, true
		}
	}
	return "", false
}

// SingleMemberID returns the caller's member id WHEN they belong to exactly one workspace.
//
// ⚠ DO NOT CALL THIS FROM A HANDLER — same ban, same rule, same reason as SingleWorkspace above.
// This docstring used to read "convenience for attributing an action in the common
// single-workspace Docs case", and MEASURED at 11c234b that convenience was invisible: a real
// handler attributing created_by from `actor, _ := authz.SingleMemberID(ctx)` passed gofmt,
// go vet, the pinned semgrep scan and the whole real-Postgres suite. Use
// permission.ActorFromContext on a gated route, or AuthorizeWorkspace's Membership.MemberID on
// a workspace-level one.
//
// It survives as ActorOrEmpty's implementation and has no other caller in the tree.
func SingleMemberID(ctx context.Context) (string, bool) {
	ms, ok := Memberships(ctx)
	if !ok || len(ms) != 1 {
		return "", false
	}
	return ms[0].MemberID, true
}

// WorkspaceOrEmpty returns the caller's single workspace, or "" if they have none or several.
//
// ⚠ BANNED, AND ITS DOCSTRING USED TO SAY THE OPPOSITE: "Handler convenience for 'override a
// client-supplied workspace with the verified one when we have an unambiguous one' — the secure
// default for create-style routes." It is the reverse of secure. `if ws := WorkspaceOrEmpty(ctx);
// ws != "" { in.WorkspaceID = ws }` is a SILENT NO-OP for every multi-workspace member, so the
// override that block exists to perform never happens and the request BODY names the tenant.
// docs-no-ambiguous-actor-helpers rejects every call. Zero callers in the tree.
//
// ⚠ AND THAT IS TRUE OF ALL FOUR: measured at 11c234b, no file outside this one calls
// SingleWorkspace, SingleMemberID, WorkspaceOrEmpty or ActorOrEmpty — every other occurrence in
// the repository is a comment, a test's comment, or the semgrep fixture's deliberate violation —
// and the two roots are called only from the two wrappers, which are called from nothing. So the
// set is reachable from no shipped code path. Whether it should be DELETED rather than banned at
// the call site is left as a decision: removing exported API is wider than the guard blindness
// this merge repairs, and the fixture cases that hold the rule open are written against these
// names.
func WorkspaceOrEmpty(ctx context.Context) string {
	ws, _ := SingleWorkspace(ctx)
	return ws
}

// ActorOrEmpty returns the caller's single member id, or "" if none/ambiguous.
//
// ⚠ BANNED, same rule, and its docstring used to call it "the verified actor that replaces the
// spoofable X-Member-Id in attribution" — which is what made it dangerous to read. For a
// multi-workspace member it returns "", and the handlers that fell back to the body when it did
// were forging identity for exactly those callers: internal/comment (a two-workspace member
// posted a comment authored as anyone, and Store.Delete gates on "only the author can delete")
// and internal/pagelock (the same member unlocked another member's lock by naming them). Both
// records are in those packages' own comments. Zero callers in the tree.
func ActorOrEmpty(ctx context.Context) string {
	m, _ := SingleMemberID(ctx)
	return m
}

// AuthorizeWorkspace authorizes a CALLER-SUPPLIED workspace id (a JSON-RPC tool arg, or one
// resolved from the object a tool touches) against the verified caller's memberships. Returns
// the matching Membership (so the caller gets the resolved member id as the actor) and ok=false
// when the caller is not a member. Fail-closed: an empty id, or no memberships in context (the
// request never passed the boundary), → ok=false. This is the MCP arg-trust cure — the workspace
// acted on is authorized against membership, never trusted from the arg.
func AuthorizeWorkspace(ctx context.Context, workspaceID string) (Membership, bool) {
	if workspaceID == "" {
		return Membership{}, false
	}
	ms, ok := Memberships(ctx)
	if !ok {
		return Membership{}, false
	}
	for _, m := range ms {
		if m.WorkspaceID == workspaceID {
			return m, true
		}
	}
	return Membership{}, false
}

// WithAuthorized returns a context carrying an authorized workspace + the caller's member id
// there — the MCP chokepoint installs it after AuthorizeWorkspace passes, so tools attribute
// writes (created_by/updated_by/verified_by) to the verified actor, not a client-supplied arg.
//
// ⚠ IT COPIES `base` AND THEREFORE COPIES `passed`. On a context that never came through the
// boundary that field stays false, which is what keeps Memberships and Email honest — see the
// block on authCtx.passed. It deliberately does NOT refuse a bare context: AuthorizedWorkspace and
// AuthorizedMember are the values this function exists to carry, and a caller that installs them
// without a boundary gets exactly those and nothing else.
func WithAuthorized(ctx context.Context, workspaceID, memberID string) context.Context {
	base, _ := ctx.Value(ctxKey{}).(*authCtx)
	next := authCtx{}
	if base != nil {
		next = *base
	}
	next.authWS, next.authMember = workspaceID, memberID
	return context.WithValue(ctx, ctxKey{}, &next)
}

// AuthorizedMember returns the resolved actor from the last AuthorizeWorkspace. ok=false if none.
func AuthorizedMember(ctx context.Context) (string, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*authCtx)
	if !ok || ac.authMember == "" {
		return "", false
	}
	return ac.authMember, true
}

// AuthorizedWorkspace returns the workspace the last AuthorizeWorkspace VERIFIED for this call — for the
// MCP chokepoint, the space's workspace it resolved and authorized the caller's membership on (stashed by
// WithAuthorized, symmetric to AuthorizedMember). ok=false if none. This is the only trustworthy tenancy
// for a created object: it is derived from the verified chokepoint, never a client-supplied workspace_id.
func AuthorizedWorkspace(ctx context.Context) (string, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*authCtx)
	if !ok || ac.authWS == "" {
		return "", false
	}
	return ac.authWS, true
}

// WithMemberships returns a context carrying a verified identity + memberships. The middleware
// installs it after resolution; handler tests use it to exercise a handler without the full
// middleware chain.
func WithMemberships(ctx context.Context, email string, ms []Membership) context.Context {
	return context.WithValue(ctx, ctxKey{}, &authCtx{email: email, memberships: ms, passed: true})
}

// Middleware resolves the gatewayauth-verified identity to workspace memberships and puts them
// in context. A verified request with no email → 403 (cannot resolve). A verified email that
// resolves to NO memberships still proceeds with an empty set — every by-id query then denies
// (ANY(empty) matches nothing) → 404, never an open read. exempt mirrors gatewayauth.
func Middleware(resolver Resolver, exempt func(path string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			id, ok := gatewayauth.IdentityFrom(r.Context())
			if !ok || id.Email == "" {
				forbidden(w)
				return
			}
			memberships, err := resolver.MembershipsByEmail(r.Context(), id.Email)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"membership resolution failed","code":"AUTHZ_ERROR"}`))
				return
			}
			next.ServeHTTP(w, r.WithContext(WithMemberships(r.Context(), id.Email, memberships)))
		})
	}
}

func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"no verified identity","code":"IDENTITY_REQUIRED"}`))
}
