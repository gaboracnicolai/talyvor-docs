// Package spaceauth authorizes resource access whose TARGET arrives in a request BODY or multipart FORM
// rather than the URL, so permission.Enforcer.Require / SpaceResolverFromParam (which read a chi URL
// param) cannot reach it. Two body/form-named gates share the same rule engine the REST enforcers and
// the MCP/collab gates use (no new access model — a thin composition of existing primitives):
//   - AuthorizeSpaceWrite: create page content in a body/form-named SPACE — requires the space's
//     AccessEdit (templatelib.Use, importer);
//   - AuthorizePageRead: read a body-named PAGE's content — requires the page's AccessView
//     (templatelib.FromPage, which copies the source page's content verbatim into a template).
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

// Authorizer resolves + tier-checks a body/form-named target space (write) or page (read).
type Authorizer struct {
	spaces   spaceLookup
	perm     *permission.Store
	pageMeta func(ctx context.Context, pageID string) (permission.PageMeta, error) // for AuthorizePageRead; nil ⇒ page-read fails closed
}

// New wires the scoped space lookup + the permission store. main.go passes *space.Store + *permission.Store.
func New(spaces spaceLookup, perm *permission.Store) *Authorizer {
	return &Authorizer{spaces: spaces, perm: perm}
}

// WithPageMeta attaches the scoped page-meta looker (page+space join, the same one that backs the REST
// page enforcer) that AuthorizePageRead needs. Optional: callers that only do AuthorizeSpaceWrite (the
// importer) omit it, and AuthorizePageRead then fails closed. Returns the receiver for chaining.
func (a *Authorizer) WithPageMeta(fn func(ctx context.Context, pageID string) (permission.PageMeta, error)) *Authorizer {
	a.pageMeta = fn
	return a
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

// AuthorizePageRead resolves whether the VERIFIED caller may READ pageID's content. found=false ⇒ the
// page is not in the caller's workspaces (handler → 404, no oracle); canView reports whether the caller
// holds >= AccessView on it (else 403). FAIL-CLOSED: a nil authorizer, no page-meta looker, empty id, a
// foreign/unresolvable page, an unresolvable actor, or any CheckPage error → found=false or canView=false,
// so content is never lifted out of a page the caller cannot read.
func (a *Authorizer) AuthorizePageRead(ctx context.Context, pageID string) (found, canView bool) {
	if a == nil || a.pageMeta == nil || pageID == "" {
		return false, false
	}
	wsIDs := authz.WorkspaceIDs(ctx)
	md, err := a.pageMeta(ctx, pageID)
	if err != nil {
		return false, false // page not in the caller's workspaces (or lookup error) → 404
	}
	actor, ok := authz.MemberIDForWorkspace(ctx, md.WorkspaceID)
	if !ok || actor == "" {
		return true, false // found, but no actor → 403
	}
	lvl, err := a.perm.CheckPage(ctx, actor, pageID, md, wsIDs)
	if err != nil {
		return true, false // tier unresolved → 403
	}
	return true, permission.AtLeast(lvl, permission.AccessView)
}
