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

// levelCtxKey carries the access level RequireAccess resolved for this request.
type levelCtxKey struct{}

// withLevel stashes the resolved level on the request context.
func withLevel(ctx context.Context, l AccessLevel) context.Context {
	return context.WithValue(ctx, levelCtxKey{}, l)
}

// LevelFromContext returns the access level RequireAccess resolved for the caller on
// the resource this route targets, computed from the GATEWAY-VERIFIED identity against
// the permission model. ok is false when the route was mounted without an Enforcer, so
// callers MUST fail closed on !ok rather than assume privilege.
//
// This exists so a handler can ask "is this caller an admin of this resource?" without
// re-resolving, and — critically — without taking the caller's word for it. Before it,
// pagelock's Unlock read `is_admin` out of the request BODY, which let any Edit-tier
// member steal another member's lock with {"is_admin": true} while simultaneously
// denying the override to real admins who did not lie about themselves.
func LevelFromContext(ctx context.Context) (AccessLevel, bool) {
	l, ok := ctx.Value(levelCtxKey{}).(AccessLevel)
	return l, ok
}

// IsAdminFromContext reports whether the verified caller holds admin on the resource
// this route targets. Fails closed: an unguarded mount (no Enforcer → no level in
// context) is not admin.
func IsAdminFromContext(ctx context.Context) bool {
	l, ok := LevelFromContext(ctx)
	return ok && l == AccessAdmin
}

// actorCtxKey carries the member id RequireAccess resolved for this request, in the
// workspace that owns the targeted resource. wsCtxKey carries that workspace.
type actorCtxKey struct{}
type wsCtxKey struct{}

func withActor(ctx context.Context, memberID string) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, memberID)
}

func withWorkspace(ctx context.Context, wsID string) context.Context {
	return context.WithValue(ctx, wsCtxKey{}, wsID)
}

// WorkspaceFromContext returns the workspace that OWNS the resource this route targets,
// as resolved by RequireAccess from the resource itself (not from anything the client
// sent). ok=false on an unguarded mount, so callers MUST fail closed.
//
// This is what lets a create-style handler DERIVE its tenant instead of requiring one:
// page.Create used to take workspace_id from the request body and page.Store.Create
// *required* it, so a caller could name any workspace and plant a row in another tenant.
// The parent resource's workspace is the only trustworthy answer, and the middleware has
// already authorized the caller against it.
func WorkspaceFromContext(ctx context.Context) (string, bool) {
	w, ok := ctx.Value(wsCtxKey{}).(string)
	return w, ok && w != ""
}

// ActorFromContext returns the caller's member id IN THE WORKSPACE THAT OWNS the
// resource this route targets, resolved by RequireAccess from the gateway-verified
// membership set. ok=false when the route was mounted without an Enforcer, so callers
// MUST fail closed on !ok rather than fall back to a client-supplied value.
//
// This is the cure for the `memberFromReq(r, in.MemberID)` pattern. That helper preferred
// authz.ActorOrEmpty but fell back to the request BODY when it was empty — and it was
// empty for every caller with != 1 memberships, which made the body authoritative for
// exactly those callers (a two-workspace member could unlock another member's lock by
// naming them in member_id). Unlike ActorOrEmpty, this is correct for ANY membership
// count, so handlers need no fallback at all.
func ActorFromContext(ctx context.Context) (string, bool) {
	m, ok := ctx.Value(actorCtxKey{}).(string)
	return m, ok && m != ""
}

// RequireAccess returns chi middleware that gates a resource route on the caller's WITHIN-workspace
// access. The caller is the gateway-verified session member — never a header, never a body field.
// Two deny statuses, composing cleanly with the SEC-4 L2 layer:
//   - the resolver can't resolve the resource in the caller's verified workspace(s) → 404 (exactly
//     like the by-id L2 layer: never leak the existence of an out-of-workspace resource);
//   - resolved but the member lacks minAccess → 403 (authenticated-but-unauthorized).
//
// ACTOR RESOLUTION (the root fix). This used to call authz.ActorOrEmpty, which delegates to
// SingleMemberID and returns "" for ANY caller whose membership count != 1. That silently made a
// member of two workspaces a non-entity: resolveAccess skips the creator rule for an empty
// memberID and no subject_type='member' grant can match "", so their real grants evaporated and
// they collapsed to the `everyone`/public-space default. It also made every
// `memberFromReq(r, in.MemberID)` body-fallback live for exactly those callers, handing the
// request body the power to name the actor.
//
// The actor is now resolved PER-RESOURCE-WORKSPACE: MemberIDForWorkspace(ctx, res.WorkspaceID) —
// the caller's member id in the workspace that owns the resource, from the verified membership
// set. Correct for any membership count. Fail-closed: a resource whose workspace the caller does
// not belong to (or a resolver that left WorkspaceID empty) yields no member id, and an empty
// actor is denied outright below rather than evaluated as an anonymous member.
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
			memberID, ok := authz.MemberIDForWorkspace(r.Context(), res.WorkspaceID)
			if !ok || memberID == "" {
				// The caller is not a member of the workspace owning this resource, or the
				// host's resolver failed to populate WorkspaceID. Either way we cannot name
				// the actor, so we cannot authorize one. Deny.
				writeForbidden(w)
				return
			}
			level, err := store.Check(r.Context(), memberID, res, authz.WorkspaceIDs(r.Context()))
			if err != nil {
				writeForbidden(w)
				return
			}
			if rank(level) < rank(minAccess) {
				writeForbidden(w) // in my workspace, but under-privileged → 403
				return
			}
			// Carry the resolved actor + level forward. The middleware has already done the
			// only trustworthy version of both computations; handlers MUST read them from
			// here (permission.ActorFromContext / IsAdminFromContext) rather than trust a
			// self-asserted field in the request body.
			ctx := withWorkspace(withActor(withLevel(r.Context(), level), memberID), res.WorkspaceID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Enforcer binds a Store + a ResourceResolver so routes can request a level via Enforcer.Require.
// A nil *Enforcer FAILS CLOSED — every route it wraps denies (404). See Require.
type Enforcer struct {
	store    *Store
	resolver ResourceResolver
}

// NewEnforcer builds an Enforcer for a resource resolver (e.g. PageResolverFromParam).
func NewEnforcer(store *Store, resolver ResourceResolver) *Enforcer {
	return &Enforcer{store: store, resolver: resolver}
}

// Require returns middleware gating the route at minAccess.
//
// A nil receiver FAILS CLOSED: it denies every wrapped request with 404 (the no-oracle convention
// RequireAccess uses for a resource outside the caller's workspace). This mirrors collab's
// WithPageScope (inScope defaults false). It was previously pass-through, so a dropped WithAccess
// line silently unguarded every route — turning by-id writes behind these gates into live
// cross-tenant writes. Fail-closed makes a missing gate a total denial (loud) rather than a silent
// hole. A route that must be public is mounted WITHOUT .With(enf.Require(...)), never with a nil
// enforcer.
func (e *Enforcer) Require(minAccess AccessLevel) func(http.Handler) http.Handler {
	if e == nil {
		return func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeNotFound(w)
			})
		}
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
			Type:        ResourceSpace,
			ID:          id,
			WorkspaceID: md.WorkspaceID,
			Private:     md.Private,
			CreatedBy:   md.CreatedBy,
		}, nil
	}
}

// SpaceLookup + PageLookup are the narrow shapes the resolvers
// expect from the host. The host wires its existing stores into
// these signatures so the permission package never imports them.
type SpaceLookup func(ctx context.Context, spaceID string) (SpaceMeta, error)
type PageLookup func(ctx context.Context, pageID string) (PageMeta, error)

type SpaceMeta struct {
	// WorkspaceID is the workspace that OWNS this space. RequireAccess resolves the
	// caller's member id in THIS workspace (authz.MemberIDForWorkspace) to evaluate
	// access — see the root fix note on RequireAccess. The host's looker must populate
	// it; an empty value fails closed (the caller resolves to no member id).
	WorkspaceID string
	Private     bool
	CreatedBy   string
}

type PageMeta struct {
	// WorkspaceID is the workspace that OWNS this page. See SpaceMeta.WorkspaceID.
	WorkspaceID    string
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
			Type:        ResourcePage,
			ID:          id,
			WorkspaceID: md.WorkspaceID,
			SpaceID:     md.SpaceID,
			Private:     md.SpacePrivate,
			CreatedBy:   md.SpaceCreatedBy,
			SpacePerms:  spacePerms,
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
			Type: ResourcePage, ID: pageID, WorkspaceID: md.WorkspaceID, SpaceID: md.SpaceID,
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
