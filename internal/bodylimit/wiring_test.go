package bodylimit_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/bodylimit"
)

// The unit tests above prove the middleware. This proves the WIRING, because chi middleware
// COMPOSES rather than overrides: an inner r.Use adds to the inherited chain, it does not
// replace it. A naive
//
//	r.Route("/v1", func(r chi.Router) {
//	    r.Use(bodylimit.Middleware(4MB))          // outer
//	    r.Group(func(r chi.Router) {
//	        r.Use(bodylimit.Middleware(200MB))    // "override" — is NOT one
//	        importerHandler.Mount(r)
//	    })
//	})
//
// runs the 4MB cap FIRST and rejects every legitimate Confluence/Notion import at 4MB. The
// bigger cap never gets a say. The exempt predicate (mirroring gatewayauth's convention) is
// what actually lets one route group opt out.

func mountLikeMain(t *testing.T, normalMax, importMax int64) http.Handler {
	t.Helper()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(bodylimit.Middleware(normalMax, func(p string) bool {
			return strings.HasPrefix(p, "/v1/import/")
		}))
		r.Post("/pages", ok)
		r.Group(func(r chi.Router) {
			r.Use(bodylimit.Middleware(importMax, nil))
			r.Post("/import/notion", ok)
		})
	})
	return r
}

func post(chain http.Handler, path string, n int) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("a", n)))
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	return rr.Code
}

func TestWiring_NormalRouteCapped(t *testing.T) {
	chain := mountLikeMain(t, 1024, 64*1024)

	if code := post(chain, "/v1/pages", 500); code != http.StatusOK {
		t.Errorf("under-cap /v1 body = %d, want 200", code)
	}
	if code := post(chain, "/v1/pages", 4096); code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-cap /v1 body = %d, want 413", code)
	}
}

// THE ONE THAT CATCHES THE COMPOSITION BUG: a body above the normal cap but below the
// import cap must be ACCEPTED on an import route.
func TestWiring_ImportRouteGetsItsLargerCap(t *testing.T) {
	chain := mountLikeMain(t, 1024, 64*1024)

	if code := post(chain, "/v1/import/notion", 4096); code != http.StatusOK {
		t.Errorf("a 4096-byte import body = %d, want 200. It is over the 1024 normal cap but well "+
			"under the 65536 import cap — a 413 here means the outer cap ran first and the import "+
			"override never applied, so every real Confluence/Notion export is rejected.", code)
	}
}

// The import cap is a cap, not an exemption from capping.
func TestWiring_ImportRouteStillCappedAtItsOwnLimit(t *testing.T) {
	chain := mountLikeMain(t, 1024, 64*1024)

	if code := post(chain, "/v1/import/notion", 128*1024); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an import body over the IMPORT cap = %d, want 413 — exempting the route from the "+
			"normal cap must not exempt it from all bounds", code)
	}
}
