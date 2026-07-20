// Package spaceauth authorizes page-content writes whose TARGET SPACE arrives in a request BODY or
// multipart FORM rather than the URL — template instantiation (templatelib.Use) and bulk import
// (importer). permission.Enforcer.Require / SpaceResolverFromParam gate a space named by a chi URL
// param; these routes name it in the body/form, so the URL-param resolver cannot reach it. This is the
// non-URL analog: it resolves the space scoped to the caller's VERIFIED workspaces and runs the SAME
// permission rule engine the REST enforcers and the MCP/collab gates use (permission.CheckSpace). No new
// access model — a thin composition of existing primitives.
package spaceauth

import (
	"context"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/permission"
)

// spaceLookup resolves a space scoped to the caller's verified workspaces. *space.Store satisfies it
// (GetByIDInWorkspaces returns ErrNotFound-style error for a space outside those workspaces).
type spaceLookup interface {
	GetByIDInWorkspaces(ctx context.Context, id string, wsIDs []string) (*model.Space, error)
}

// Decision is the outcome of authorizing a page-content write into a space.
type Decision struct {
	// WorkspaceID is the space's OWNING workspace — the tenancy the new content must carry (a page
	// belongs to its space's workspace). Callers use this, never a client-supplied workspace id.
	WorkspaceID string
	// Actor is the caller's member id in that workspace (attribution), correct for any membership count.
	Actor string
	// CanEdit reports whether the caller holds >= AccessEdit on the space — the tier a content write needs.
	CanEdit bool
	// Found reports whether the space is in the caller's verified workspaces. false ⇒ the handler must
	// answer 404 (no existence oracle), exactly like the by-id L2 layer.
	Found bool
}

// Authorizer resolves + tier-checks a body/form-named target space.
type Authorizer struct {
	spaces spaceLookup
	perm   *permission.Store
}

// New wires the scoped space lookup + the permission store. main.go passes *space.Store + *permission.Store.
func New(spaces spaceLookup, perm *permission.Store) *Authorizer {
	return &Authorizer{spaces: spaces, perm: perm}
}

// AuthorizeSpaceWrite resolves whether the VERIFIED caller may create page content in spaceID. FAIL-CLOSED
// throughout: a nil authorizer, a foreign/unresolvable space, or any lookup error yields Found=false or
// CanEdit=false — a page is never created on an unresolved or under-privileged space.
func (a *Authorizer) AuthorizeSpaceWrite(ctx context.Context, spaceID string) Decision {
	if a == nil || spaceID == "" {
		return Decision{}
	}
	wsIDs := authz.WorkspaceIDs(ctx)
	sp, err := a.spaces.GetByIDInWorkspaces(ctx, spaceID, wsIDs)
	if err != nil || sp == nil {
		return Decision{} // space not in the caller's workspaces (or lookup error) → Found=false → 404
	}
	actor, ok := authz.MemberIDForWorkspace(ctx, sp.WorkspaceID)
	if !ok || actor == "" {
		return Decision{WorkspaceID: sp.WorkspaceID, Found: true} // no actor → CanEdit=false → 403
	}
	lvl, err := a.perm.CheckSpace(ctx, actor, spaceID, permission.SpaceMeta{
		WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy,
	}, wsIDs)
	if err != nil {
		return Decision{WorkspaceID: sp.WorkspaceID, Actor: actor, Found: true} // tier unresolved → 403
	}
	return Decision{
		WorkspaceID: sp.WorkspaceID,
		Actor:       actor,
		Found:       true,
		CanEdit:     permission.AtLeast(lvl, permission.AccessEdit),
	}
}
