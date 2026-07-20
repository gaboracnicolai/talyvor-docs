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
}

// NewPermissionSession wires the permission store + the host's scoped page-meta looker into a
// SessionResolver. The looker resolves the page (and its space) scoped to the caller's workspaces, so a
// foreign page yields an error → inScope=false.
func NewPermissionSession(perm *permission.Store, pageMeta func(ctx context.Context, pageID string) (permission.PageMeta, error)) *PermissionSession {
	return &PermissionSession{perm: perm, pageMeta: pageMeta}
}

func (a *PermissionSession) ResolveSession(ctx context.Context, pageID string) (bool, string, bool) {
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
