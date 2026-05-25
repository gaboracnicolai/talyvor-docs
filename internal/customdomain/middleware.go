package customdomain

import (
	"context"
	"net/http"
	"strings"
)

// ctxKey is the unexported type used to thread custom-domain
// metadata through the request chain. Handlers downstream of
// DomainRouter pull workspace/space IDs via these keys.
type ctxKey int

const (
	ctxKeyWorkspace ctxKey = iota
	ctxKeySpace
)

// WorkspaceFromContext returns the workspace ID resolved by the
// DomainRouter middleware. Empty when the request didn't arrive on
// a custom domain — handlers should fall back to the URL param or
// header in that case.
func WorkspaceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyWorkspace).(string)
	return v
}

// SpaceFromContext mirrors WorkspaceFromContext for the optional
// per-space binding.
func SpaceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeySpace).(string)
	return v
}

// Resolver is the narrow shape DomainRouter needs from the store.
// Tests can stub this without a real DB.
type Resolver interface {
	GetByDomain(ctx context.Context, domain string) (*CustomDomain, error)
}

// DomainRouter checks the Host header against the custom_domains
// table. When a verified record matches, the request is routed to
// publicHandler (read-only public-space surface) with context
// values for workspace + space.
//
// All other requests pass through to mainHandler unchanged — the
// admin UI + API never live on a custom domain, so the wrapping is
// transparent for the default deployment.
//
// The configured listenHost (set by the server's own bind address)
// is treated as the canonical host and always falls through to
// mainHandler, regardless of any custom_domains row.
func DomainRouter(store Resolver, publicHandler, mainHandler http.Handler, listenHost string) http.Handler {
	listenHost = strings.ToLower(strings.TrimSpace(listenHost))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normaliseHost(r.Host)
		// Anything pointing at the server's own bind address always
		// reaches the main router. Skip the DB hit on that path.
		if host == "" || host == listenHost || isLocalHost(host) {
			mainHandler.ServeHTTP(w, r)
			return
		}
		cd, err := store.GetByDomain(r.Context(), host)
		if err != nil || cd == nil || !cd.Verified {
			mainHandler.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyWorkspace, cd.WorkspaceID)
		if cd.SpaceID != nil {
			ctx = context.WithValue(ctx, ctxKeySpace, *cd.SpaceID)
		}
		publicHandler.ServeHTTP(w, r.WithContext(ctx))
	})
}

// normaliseHost strips the port (Host headers in HTTP/1.x include
// it) and lowercases for the case-insensitive DB lookup.
func normaliseHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// isLocalHost catches the common "I'm running locally" hostnames so
// dev environments never accidentally treat themselves as custom
// domains. The check is heuristic — `host.docker.internal` etc.
// also benefit from skipping the DB lookup.
func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1", "host.docker.internal":
		return true
	}
	return false
}
