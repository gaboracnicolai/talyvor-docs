package collab

import (
	"context"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

// SessionResolver resolves, from the VERIFIED request context, a connecting member's relationship to
// the page a collab session targets:
//
//	inScope — the page is in the caller's verified workspaces; false ⇒ the WS entry point refuses the
//	          connection (404) — the SEC-4 membership gate;
//	actor   — the caller's member id IN THE PAGE'S workspace (authz.MemberIDForWorkspace), correct for
//	          ANY membership count — the fix for the ActorOrEmpty empty-id-for-multi-workspace residue;
//	canEdit — the caller holds >= AccessEdit on the page, the tier a `change` frame requires.
//
// All three are resolved against the roster AS IT STANDS AT THE CALL, not the one the request
// context carried at connect — dispatch calls this per `change` frame precisely so the answer can
// change under a live socket. See refreshRoster for what that costs and why it is not optional.
//
// Fail-closed: any resolution failure yields inScope=false (refuse the connection) or canEdit=false
// (connect read-only, refuse changes) — never a permissive default.
type SessionResolver interface {
	ResolveSession(ctx context.Context, pageID string) (inScope bool, actor string, canEdit bool)
}

// PermissionSession adapts the permission rule engine to SessionResolver — the same resolveAccess the
// REST enforcer and the MCP write gate run, for the non-HTTP collab boundary. It reuses the host's
// scoped page-meta looker (the page+space join that also backs the REST enforcers) plus
// authz.MemberIDForWorkspace and permission.CheckPage/AtLeast; nothing here is a new access model.
// Mirror of mcp.PermissionAccess.
type PermissionSession struct {
	perm     *permission.Store
	pageMeta func(ctx context.Context, pageID string) (permission.PageMeta, error)
	roster   authz.Resolver
}

// NewPermissionSession wires the permission store + the host's scoped page-meta looker + the
// membership resolver into a SessionResolver. The looker resolves the page (and its space) scoped to
// the caller's workspaces, so a foreign page yields an error → inScope=false.
//
// roster is REQUIRED and positional rather than a WithX option on purpose: it is what keeps the
// membership scope current for the life of a socket (see refreshRoster), and an option is a thing a
// deployment can forget. Passing nil compiles and then refuses every session — loud, not silent.
func NewPermissionSession(perm *permission.Store, pageMeta func(ctx context.Context, pageID string) (permission.PageMeta, error), roster authz.Resolver) *PermissionSession {
	return &PermissionSession{perm: perm, pageMeta: pageMeta, roster: roster}
}

// refreshRoster returns ctx with the caller's memberships RE-READ from workspace_members, so every
// resolution below is scoped by the roster as it stands NOW rather than as it stood at connect.
//
// WHY THIS EXISTS. authz.Middleware resolves memberships ONCE per HTTP request and puts them in the
// request context. For a REST call that is exact — the request is the lifetime. A collab session is
// not a request: it is a socket that outlives its own handshake indefinitely (writePump pings every
// 30s, every Pong resets readWait), and ServeWS hands `r.Context()` to a read pump that keeps using
// it. So `authz.WorkspaceIDs(ctx)` — which pageMeta scopes on, which CheckPage scopes on, and which
// MemberIDForWorkspace resolves the actor from — was a snapshot taken before the upgrade.
//
// MEASURED, not reasoned (removed_midsession_realpg_test.go): remove a mid-session member through
// membership.Store.ReconcileWorkspace — the one production writer of that table, driven by the
// trackintegration syncer, i.e. what happens when an administrator removes somebody in Track — and
// their next `change` frame was still ACKed, still broadcast, and still reached pages.content, WHILE
// a new dial by the same person was refused 404 by the same server. #123 closed this time axis for
// `permissions` and its comment named the reason: the connect-time answer is a statement about one
// instant. `workspace_members` is the other revocable table under the same socket.
//
// FAIL-CLOSED IN EVERY DIRECTION: no resolver, no verified email, or a lookup error → refuse. A
// caller who resolves to NO memberships is NOT special-cased — the rebuilt context carries an empty
// set and the existing machinery denies it (ANY(empty) matches nothing → pageMeta errors →
// inScope=false), which is authz's own documented fail-closed property rather than a second one
// written here.
func (a *PermissionSession) refreshRoster(ctx context.Context) (context.Context, bool) {
	if a.roster == nil {
		return ctx, false
	}
	email, ok := authz.Email(ctx)
	if !ok || email == "" {
		return ctx, false
	}
	ms, err := a.roster.MembershipsByEmail(ctx, email)
	if err != nil {
		return ctx, false
	}
	return authz.WithMemberships(ctx, email, ms), true
}

func (a *PermissionSession) ResolveSession(ctx context.Context, pageID string) (bool, string, bool) {
	ctx, ok := a.refreshRoster(ctx)
	if !ok {
		// Roster unreadable → refuse the connection / refuse the change. Never serve on a stale one.
		return false, "", false
	}
	md, err := a.pageMeta(ctx, pageID)
	if err != nil {
		// Page not in the caller's verified workspaces (or a lookup error) → refuse the connection.
		return false, "", false
	}
	actor, ok := authz.MemberIDForWorkspace(ctx, md.WorkspaceID)
	if !ok || actor == "" {
		// In scope but the actor cannot be resolved — connect read-only, refuse changes (fail-closed).
		return true, "", false
	}
	lvl, err := a.perm.CheckPage(ctx, actor, pageID, md, authz.WorkspaceIDs(ctx))
	if err != nil {
		// Tier unresolved → read-only (fail-closed).
		return true, actor, false
	}
	return true, actor, permission.AtLeast(lvl, permission.AccessEdit)
}
