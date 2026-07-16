package ratelimit

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
)

// WorkspaceLimit returns chi middleware that rate-limits a {param} workspace route,
// per-tenant.
//
// THE KEY IS THE VERIFIED WORKSPACE, NEVER THE RAW PARAM. This middleware runs BEFORE the
// handler, so it cannot rely on the handler's own AuthorizeWorkspace call — it authorizes
// the workspace itself and keys on the Membership it gets back. Keying on the raw
// chi.URLParam would repeat the authority-class mistake closed in #23, in a new place, and
// hand an attacker two wins:
//
//  1. EVASION — name any workspace (or junk) and spend from a fresh bucket, indefinitely,
//     by rotating the string. The limit would bound nothing.
//  2. CROSS-TENANT DoS — hammer /workspaces/{victim}/... to drain the VICTIM's bucket and
//     lock a tenant out of AI without being able to read a byte of their data.
//
// Authorizing before spending a token is what makes (2) impossible: a denied request never
// touches the bucket.
//
// Ordering: mount this INSIDE the /v1 group, i.e. after gatewayauth + authz have run.
// Without the authz middleware there are no memberships in context, AuthorizeWorkspace
// returns false, and every request 403s — fail closed, not open.
func (l *Limiter) WorkspaceLimit(param string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace; the limiter keys on the returned Membership, never on this raw param
			wsID := chi.URLParam(r, param)
			m, ok := authz.AuthorizeWorkspace(r.Context(), wsID)
			if !ok {
				// Deny on AUTHORITY before any token is spent. Deliberately 403, not 429:
				// the caller is not a member, which is not a rate condition — and spending a
				// token here is exactly the cross-tenant DoS above.
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
				return
			}
			if !l.Allow(m.WorkspaceID) { // the VERIFIED id, not wsID
				retryAfter(w, l.perSec)
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "rate limit exceeded for this workspace, please retry shortly",
					"code":  "RATE_LIMITED",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// retryAfter advertises when a token will next be available, so a client can back off
// instead of hot-looping. Seconds, rounded up, floored at 1 (RFC 9110 wants a positive
// integer; a "0" would invite an immediate retry that is certain to fail).
func retryAfter(w http.ResponseWriter, perSec float64) {
	secs := 1
	if perSec > 0 {
		if s := int(math.Ceil(1 / perSec)); s > 1 {
			secs = s
		}
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
