package bodylimit_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/bodylimit"
)

// WHAT AN EXEMPTION COSTS WHEN THE ROUTE IT EXEMPTS IS NOT IN THE GROUP THAT RE-CAPS IT.
//
// cmd/docs pairs two things that are NOT the same population:
//
//	r.Use(bodylimit.Middleware(cfg.MaxBodyBytes, func(p string) bool {   // a STRING PREFIX
//	    return strings.HasPrefix(p, "/v1/import/")
//	}))
//	...
//	r.Group(func(r chi.Router) {                                        // a ROUTE GROUP
//	    r.Use(bodylimit.Middleware(cfg.MaxImportBodyBytes, nil))
//	    importerHandler.Mount(r)
//	})
//
// The exemption is keyed on the PATH; the larger cap is keyed on MEMBERSHIP OF A GROUP. They
// coincide today at exactly two routes, and nothing said so. Middleware() returns
// `next.ServeHTTP` UNWRAPPED for an exempt path — so a route that matches the prefix but is
// mounted OUTSIDE the group is exempt from the 4MB cap and never reaches the 200MB one.
// Not re-capped. UNCAPPED.
//
// ⚠ MEASURED 2026-08-28 (tab-c5j7, W3.31), at the REAL shipped values rather than synthetic
// ones: a 4MB+1KB body to a route registered under the exempt prefix but outside the group
// was answered **200**, against **413** for the same body on a normal /v1 route. That is the
// unbounded-memory read this package was written to stop — PATCH /pages/{id} decoding a
// multi-GB body into a map[string]any, with no container memory limit behind it.
//
// ⚠ THIS IS A HAZARD TEST, NOT A BUG REPORT. No such route exists today: all routes under
// the prefix are the importer's two, and both are inside the group — pinned as a POPULATION
// by internal/mountguard/importcap_test.go, which is the guard this test exists to justify.
// The pair is deliberate: this one shows what the invariant is worth, that one enforces it.
// Neither alone would survive review of the other's absence.

func mountWithRogue(normalMax, importMax int64) http.Handler {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(bodylimit.Middleware(normalMax, func(p string) bool {
			return strings.HasPrefix(p, "/v1/import/")
		}))
		r.Post("/pages", ok)
		// Matches the exempt prefix; NOT in the group below. This is the shape the guard forbids.
		r.Post("/import/rogue", ok)
		r.Group(func(r chi.Router) {
			r.Use(bodylimit.Middleware(importMax, nil))
			r.Post("/import/notion", ok)
		})
	})
	return r
}

func postBody(h http.Handler, path string, n int) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("a", n)))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestExemptButUngrouped_IsUncapped pins the CONSEQUENCE. If this ever starts returning 413,
// the middleware's exempt contract has changed and mountguard's census may no longer be
// load-bearing — read both together before relaxing either.
func TestExemptButUngrouped_IsUncapped(t *testing.T) {
	const normal, imp = 4 << 20, 200 << 20
	over := normal + 1024

	if code := postBody(mountWithRogue(normal, imp), "/v1/pages", over); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("baseline: an over-cap body on a NORMAL /v1 route = %d, want 413. Without this "+
			"the comparison below is meaningless — it would not show an exemption, it would show "+
			"a cap that never worked.", code)
	}

	code := postBody(mountWithRogue(normal, imp), "/v1/import/rogue", over)
	if code != http.StatusOK {
		t.Fatalf("a route matching the exempt prefix but mounted OUTSIDE the import group "+
			"answered %d for a %d-byte body; this test recorded 200 (UNCAPPED) at the time it "+
			"was written. If the exempt contract now re-caps such a route, say so here — and "+
			"re-read internal/mountguard/importcap_test.go, whose whole purpose is to make this "+
			"shape impossible to add.", code, over)
	}

	// The same body on a route that IS in the group is capped by the LARGER limit, not exempt
	// from capping altogether — the distinction the census protects.
	if code := postBody(mountWithRogue(normal, imp), "/v1/import/notion", imp+1024); code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the IMPORT cap on a GROUPED import route = %d, want 413 — being "+
			"exempt from the normal cap must not mean being exempt from all bounds", code)
	}
}
