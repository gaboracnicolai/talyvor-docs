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

// RequireAccess returns chi-compatible middleware that 403s any
// request where the caller (X-Member-Id header) doesn't have at
// least minAccess on the resource.
func RequireAccess(store *Store, resolver ResourceResolver, minAccess AccessLevel) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			res, err := resolver(r.Context(), r)
			if err != nil {
				writeForbidden(w)
				return
			}
			memberID := authz.ActorOrEmpty(r.Context())
			level, err := store.Check(r.Context(), memberID, res)
			if err != nil {
				writeForbidden(w)
				return
			}
			if rank(level) < rank(minAccess) {
				writeForbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
		spacePerms, _ := store.ListForResource(ctx, ResourceSpace, md.SpaceID)
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

func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
}
