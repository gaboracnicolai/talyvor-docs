package mcp

import (
	"context"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

// AccessController answers whether the VERIFIED caller may act on an object via an MCP tool — the same
// tiers the REST doors require. The membership chokepoint in callTool has already confirmed the caller
// belongs to the acted-on workspace; this adds the WITHIN-workspace tier that membership alone does not
// check (a view-tier member must not create/update/verify a page through MCP, and a member with no grant
// on a private space must not read one).
//
// A nil controller makes every gated tool DENY (see Server.authorizeWrite / authorizeRead): a dropped
// wiring is a loud total denial, never a silent reopening of the tier hole.
//
// ⚠ THE READ HALF WAS MISSING FOR THE WHOLE LIFE OF THIS INTERFACE, AND ITS ABSENCE WAS THE DEFECT:
// authorizeWrite mapped only create/update/verify_page, so this was a WRITE gate and every read tool
// stopped at the workspace tier. Measured on real Postgres through POST /mcp — a member with no grant
// received a private space's page from search_docs, get_page (the whole content_text), list_pages,
// get_space_tree and get_page_analytics. See privatespace_realpg_test.go.
type AccessController interface {
	// CanEditPage reports whether member holds >= AccessEdit on the page (space inheritance included).
	CanEditPage(ctx context.Context, pageID, memberID string) (bool, error)
	// CanEditSpace reports whether member holds >= AccessEdit on the space.
	CanEditSpace(ctx context.Context, spaceID, memberID string) (bool, error)
	// CanViewPage reports whether member holds >= AccessView on the page (space inheritance included).
	CanViewPage(ctx context.Context, pageID, memberID string) (bool, error)
	// CanViewSpace reports whether member holds >= AccessView on the space.
	CanViewSpace(ctx context.Context, spaceID, memberID string) (bool, error)
}

// PermissionAccess adapts the permission rule engine to AccessController. It resolves the resource's
// meta scoped to the caller's verified workspaces (the host wires its page/space lookers) and runs the
// SAME resolveAccess the REST enforcer uses via permission.CheckPage / CheckSpace — no second access
// model. main.go and the MCP tests both build one from the stores they already have.
type PermissionAccess struct {
	perm      *permission.Store
	pageMeta  func(ctx context.Context, pageID string) (permission.PageMeta, error)
	spaceMeta func(ctx context.Context, spaceID string) (permission.SpaceMeta, error)
}

// NewPermissionAccess wires the permission store + the host's scoped meta lookers (the same lookers
// that back the REST enforcers) into an AccessController.
func NewPermissionAccess(
	perm *permission.Store,
	pageMeta func(ctx context.Context, pageID string) (permission.PageMeta, error),
	spaceMeta func(ctx context.Context, spaceID string) (permission.SpaceMeta, error),
) *PermissionAccess {
	return &PermissionAccess{perm: perm, pageMeta: pageMeta, spaceMeta: spaceMeta}
}

func (a *PermissionAccess) CanEditPage(ctx context.Context, pageID, memberID string) (bool, error) {
	md, err := a.pageMeta(ctx, pageID)
	if err != nil {
		return false, err
	}
	lvl, err := a.perm.CheckPage(ctx, memberID, pageID, md, authz.WorkspaceIDs(ctx))
	if err != nil {
		return false, err
	}
	return permission.AtLeast(lvl, permission.AccessEdit), nil
}

func (a *PermissionAccess) CanEditSpace(ctx context.Context, spaceID, memberID string) (bool, error) {
	md, err := a.spaceMeta(ctx, spaceID)
	if err != nil {
		return false, err
	}
	lvl, err := a.perm.CheckSpace(ctx, memberID, spaceID, md, authz.WorkspaceIDs(ctx))
	if err != nil {
		return false, err
	}
	return permission.AtLeast(lvl, permission.AccessEdit), nil
}

// CanViewPage / CanViewSpace are the same two lookups at the VIEW tier. They are separate methods
// rather than one CheckLevel(min) because the interface is what a reader of the dispatch switch sees:
// a tool named in the read set and a tool named in the write set should not differ by an argument.
func (a *PermissionAccess) CanViewPage(ctx context.Context, pageID, memberID string) (bool, error) {
	md, err := a.pageMeta(ctx, pageID)
	if err != nil {
		return false, err
	}
	lvl, err := a.perm.CheckPage(ctx, memberID, pageID, md, authz.WorkspaceIDs(ctx))
	if err != nil {
		return false, err
	}
	return permission.AtLeast(lvl, permission.AccessView), nil
}

func (a *PermissionAccess) CanViewSpace(ctx context.Context, spaceID, memberID string) (bool, error) {
	md, err := a.spaceMeta(ctx, spaceID)
	if err != nil {
		return false, err
	}
	lvl, err := a.perm.CheckSpace(ctx, memberID, spaceID, md, authz.WorkspaceIDs(ctx))
	if err != nil {
		return false, err
	}
	return permission.AtLeast(lvl, permission.AccessView), nil
}
