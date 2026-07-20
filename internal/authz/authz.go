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
}

// Memberships returns the verified caller's full membership set. ok=false when the request
// never passed the boundary.
func Memberships(ctx context.Context) ([]Membership, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*authCtx)
	if !ok {
		return nil, false
	}
	return ac.memberships, true
}

// Email returns the verified caller email. ok=false when the request never passed the boundary.
func Email(ctx context.Context) (string, bool) {
	ac, ok := ctx.Value(ctxKey{}).(*authCtx)
	if !ok {
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

// SingleWorkspace returns the caller's workspace WHEN they belong to exactly one — the Docs
// common case (one workspace per instance). ok=false for zero or multiple, so a caller that
// needs an unambiguous create-target must handle the ambiguous case explicitly.
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

// SingleMemberID returns the caller's member id WHEN they belong to exactly one workspace —
// convenience for attributing an action in the common single-workspace Docs case.
func SingleMemberID(ctx context.Context) (string, bool) {
	ms, ok := Memberships(ctx)
	if !ok || len(ms) != 1 {
		return "", false
	}
	return ms[0].MemberID, true
}

// WorkspaceOrEmpty returns the caller's single workspace, or "" if they have none or several.
// Handler convenience for "override a client-supplied workspace with the verified one when we
// have an unambiguous one" — the secure default for create-style routes.
func WorkspaceOrEmpty(ctx context.Context) string {
	ws, _ := SingleWorkspace(ctx)
	return ws
}

// ActorOrEmpty returns the caller's single member id, or "" if none/ambiguous — the verified
// actor that replaces the spoofable X-Member-Id in attribution.
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
	return context.WithValue(ctx, ctxKey{}, &authCtx{email: email, memberships: ms})
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
