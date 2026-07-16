// Package bodylimit caps the size of request bodies.
//
// Before this, no route in Docs bounded its request body: every handler is
// `json.NewDecoder(r.Body).Decode(&in)` and http.MaxBytesReader appeared nowhere in the
// repository. PATCH /pages/{id} decodes into a map[string]any, so a multi-gigabyte
// `content` field was read fully into RAM, written to Postgres, walked by
// extractContentText, and then fanned to the semantic indexer — and docker-compose sets no
// memory limit on the container.
//
// TWO LAYERS, because one is not enough:
//
//  1. A Content-Length check → 413 before the handler runs. Every ordinary client (the SPA,
//     curl, fetch) declares a length, so this is the path that matters in practice, and it
//     is the only one that can produce a clean, specific status.
//
//  2. http.MaxBytesReader for everything else. A client that omits Content-Length (chunked)
//     or understates it cannot be pre-screened, so the body is wrapped and the read simply
//     stops at the cap. The handler's existing decode-error path then answers — a 400
//     rather than a 413, which is less precise but leaves the SECURITY property intact:
//     memory is bounded. Making that case 413 too would mean touching the error handling of
//     every decode site in the codebase; the honest trade is a slightly wrong status on a
//     path only a hostile or exotic client takes.
//
// This is a memory bound, not a business rule. It is deliberately far above any legitimate
// payload — see cmd/docs for the sizing and the importer's separate, larger cap.
package bodylimit

import (
	"encoding/json"
	"net/http"
)

// Middleware caps request bodies at max bytes. A non-positive max disables the cap and is
// treated as a programming error by the caller rather than silently allowing everything —
// cmd/docs never passes one.
//
// exempt skips the cap for paths that carry their own (the importer's ZIP routes). It
// exists because chi middleware COMPOSES: an inner r.Use adds to the inherited chain rather
// than replacing it, so a "bigger cap" registered on a sub-group runs AFTER the outer one
// and never gets a say — every real import would be rejected at the outer limit. A nil
// exempt caps everything. Mirrors gatewayauth.Middleware's exempt convention.
func Middleware(max int64, exempt func(path string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r.URL.Path) {
				next.ServeHTTP(w, r) // a sub-group applies its own, larger cap
				return
			}
			if max <= 0 || r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			// Layer 1: an honest client that declares an oversize body is rejected before
			// the handler allocates anything.
			if r.ContentLength > max {
				writeTooLarge(w, max)
				return
			}
			// Layer 2: unknown or understated length — bound the read itself. MaxBytesReader
			// also closes the connection on overflow, so a hostile client cannot hold the
			// socket open streaming forever.
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

func writeTooLarge(w http.ResponseWriter, max int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":     "request body too large",
		"code":      "BODY_TOO_LARGE",
		"max_bytes": max,
	})
}
