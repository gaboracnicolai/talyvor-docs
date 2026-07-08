package permission

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
)

// ResourceResolver fetches a resourceContext for the resource the
// request targets. Implementations are provided by the host: a page
// resolver joins pages + spaces to populate inheritance; a space
// resolver is a single SELECT.
//
// We deliberately don't have the permission package own the lookup
// — pages and spaces live in other packages, and pulling them in
// here would create a dependency cycle.
type ResourceResolver func(ctx context.Context, r *http.Request) (resourceContext, error)

// RequireAccess returns chi middleware that gates a resource route on the caller's WITHIN-workspace
// access. The caller is the gateway-verified session member (authz.ActorOrEmpty — never a header).
// Two deny statuses, composing cleanly with the SEC-4 L2 layer:
//   - the resolver can't resolve the resource in the caller's verified workspace(s) → 404 (exactly
//     like the by-id L2 layer: never leak the existence of an out-of-workspace resource);
//   - resolved but the member lacks minAccess → 403 (authenticated-but-unauthorized).
func RequireAccess(store *Store, resolver ResourceResolver, minAccess AccessLevel) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res, err := resolver(r.Context(), r)
			if err != nil {
				// Resource not found in the caller's workspace set (the host's resolver scopes its
				// lookup to authz.WorkspaceIDs). 404 — same as the L2 layer, no existence oracle.
				writeNotFound(w)
				return
			}
			memberID := authz.ActorOrEmpty(r.Context())
			level, err := store.Check(r.Context(), memberID, res, authz.WorkspaceIDs(r.Context()))
			if err != nil {
				writeForbidden(w)
				return
			}
			if rank(level) < rank(minAccess) {
				writeForbidden(w) // in my workspace, but under-privileged → 403
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Enforcer binds a Store + a ResourceResolver so routes can request a level via Enforcer.Require.
// A nil *Enforcer yields pass-through middleware — handlers stay mountable unguarded (tests).
type Enforcer struct {
	store    *Store
	resolver ResourceResolver
}

// NewEnforcer builds an Enforcer for a resource resolver (e.g. PageResolverFromParam).
func NewEnforcer(store *Store, resolver ResourceResolver) *Enforcer {
	return &Enforcer{store: store, resolver: resolver}
}

// Require returns middleware gating the route at minAccess. Nil receiver → pass-through.
func (e *Enforcer) Require(minAccess AccessLevel) func(http.Handler) http.Handler {
	if e == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return RequireAccess(e.store, e.resolver, minAccess)
}

// SpaceResolverFromParam pulls space metadata from the URL via the
// supplied lookup. The looker accepts the space ID and returns
// private + created_by — the only fields the rule engine needs.
func SpaceResolverFromParam(paramName string, looker SpaceLookup) ResourceResolver {
	return func(ctx context.Context, r *http.Request) (resourceContext, error) {
		id := chi.URLParam(r, paramName)
		md, err := looker(ctx, id)
		if err != nil {
			return resourceContext{}, err
		}
		return resourceContext{
			Type:      ResourceSpace,
			ID:        id,
			Private:   md.Private,
			CreatedBy: md.CreatedBy,
		}, nil
	}
}

// SpaceLookup + PageLookup are the narrow shapes the resolvers
// expect from the host. The host wires its existing stores into
// these signatures so the permission package never imports them.
type SpaceLookup func(ctx context.Context, spaceID string) (SpaceMeta, error)
type PageLookup func(ctx context.Context, pageID string) (PageMeta, error)

type SpaceMeta struct {
	Private   bool
	CreatedBy string
}

type PageMeta struct {
	SpaceID        string
	SpaceCreatedBy string
	SpacePrivate   bool
	PageCreatedBy  string
}

// PageResolverFromParam joins the page + its space so the page's
// resourceContext carries inherited space permissions. The store
// preloads the space-level grants via ListForResource.
func PageResolverFromParam(paramName string, looker PageLookup, store *Store) ResourceResolver {
	return func(ctx context.Context, r *http.Request) (resourceContext, error) {
		id := chi.URLParam(r, paramName)
		md, err := looker(ctx, id)
		if err != nil {
			return resourceContext{}, err
		}
		// Page inherits its space's privacy + creator-admin rule.
		// We use the SPACE creator (not the page creator) for the
		// default-admin contract — admins of a space should
		// transitively admin every page in it.
		spacePerms, _ := store.ListForResource(ctx, ResourceSpace, md.SpaceID, authz.WorkspaceIDs(ctx))
		return resourceContext{
			Type:       ResourcePage,
			ID:         id,
			SpaceID:    md.SpaceID,
			Private:    md.SpacePrivate,
			CreatedBy:  md.SpaceCreatedBy,
			SpacePerms: spacePerms,
		}, nil
	}
}

// BlockPageLookup resolves a block id to its owning page id + that page's meta, for gating the
// by-block-id routes (/blocks/{blockID}) — blocks are page content, so they inherit the page's access.
// The host scopes its lookup to the caller's workspaces (a foreign block → error → 404).
type BlockPageLookup func(ctx context.Context, blockID string) (pageID string, md PageMeta, err error)

// PageResolverFromBlock gates a /blocks/{blockID} route on the access of the PAGE that owns the block.
func PageResolverFromBlock(blockParam string, looker BlockPageLookup, store *Store) ResourceResolver {
	return func(ctx context.Context, r *http.Request) (resourceContext, error) {
		pageID, md, err := looker(ctx, chi.URLParam(r, blockParam))
		if err != nil {
			return resourceContext{}, err
		}
		spacePerms, _ := store.ListForResource(ctx, ResourceSpace, md.SpaceID, authz.WorkspaceIDs(ctx))
		return resourceContext{
			Type: ResourcePage, ID: pageID, SpaceID: md.SpaceID,
			Private: md.SpacePrivate, CreatedBy: md.SpaceCreatedBy, SpacePerms: spacePerms,
		}, nil
	}
}

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
}

func writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
}
