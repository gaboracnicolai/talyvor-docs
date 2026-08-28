package gatewayauth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/gatewayauth"
)

// WHY A PATH-PREFIX EXEMPTION IS SAFE HERE, PINNED RATHER THAN ASSUMED.
//
// ExemptTransitProof answers on r.URL.Path — the DECODED but UNCLEANED request path. cmd/docs
// installs no path-normalising middleware (no CleanPath, no StripSlashes), so a request may
// present `/v1/public/../spaces`: the predicate sees the `/v1/public/` prefix and exempts it
// from the gateway secret. If the router then resolved `..` and dispatched to /v1/spaces, that
// handler would have been reached with no secret at all.
//
// ⚠ MEASURED 2026-08-28 (tab-c5j7, W3.32): it does not. chi matches path segments literally and
// never resolves `..`, so every such request 404s at the router. Nine probes — `..`, `%2e%2e`,
// `..%2f`, `./..`, a leading `//`, and traversal out of the share route's own subtree — and not
// one reached a protected handler. The exemption is safe because of how chi routes, NOT because
// the predicate is careful.
//
// ⚠⚠ THAT IS A PROPERTY OF A DEPENDENCY, WHICH IS PRECISELY WHY IT IS PINNED HERE. The safety of
// an auth exemption should not rest on an unstated assumption about a third-party router. If chi
// ever normalises, or if someone adds a normaliser that runs BEFORE these middlewares in a way
// that re-introduces the prefix, this test goes red and the boundary gets re-examined by a human
// — which is the only correct outcome on this path.
//
// Controls: ~/talyvor-queue/w332-authexempt-controls-c5j7.py — 8 rows, 0 miss. C8 is this file's:
// installing a NORMALISING router (chi's CleanPath) resolves `..`, the exempt path then reaches a
// protected handler, and this test goes RED. So it is armed against the exact future in which the
// assumption above stops holding, rather than merely agreeing with today's chi.
//
// ⚠ THE CONTROL IS IN THE TABLE, NOT BESIDE IT: the `/v1/spaces` row must reach the protected
// handler. Without a probe that CAN reach one, a wall of "did not reach" is indistinguishable
// from a router that serves nothing at all, and this test would pass with the routes deleted.

func TestExemptPrefix_CannotBeTraversedIntoAProtectedRoute(t *testing.T) {
	const reached = "PROTECTED"
	protected := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(reached)) }
	public := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("public")) }

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Get("/public/s/{token}", public)
		r.Get("/spaces", protected)
		r.Get("/workspaces/{wsID}/pages", protected)
	})

	cases := []struct {
		path            string
		wantExempt      bool
		wantReachedProt bool
		why             string
	}{
		{"/v1/spaces", false, true,
			"THE CONTROL. A protected route must be reachable and must NOT be exempt; without " +
				"this row every 'did not reach' below could just mean nothing is mounted."},
		{"/v1/public/s/abc", true, false,
			"the share viewer: exempt by design, and it is not a protected handler"},
		{"/v1/public/../spaces", true, false, "literal .. segment"},
		{"/v1/public/%2e%2e/spaces", true, false, "percent-encoded .."},
		{"/v1/public/..%2fspaces", true, false, "encoded slash after .."},
		{"/v1/public/./../spaces", true, false, "./.. mix"},
		{"/v1/public/../workspaces/W1/pages", true, false, "traversal into a param route"},
		{"/v1/public/s/../../spaces", true, false, "traversal out of the share route's subtree"},
		{"//v1/public/../spaces", false, false,
			"a leading // does not even match the exempt prefix — recorded so the row is not " +
				"mistaken for a traversal that was blocked by the router"},
	}

	sawControl := false
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		gotExempt := gatewayauth.ExemptTransitProof(req.URL.Path)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		gotReached := rr.Body.String() == reached

		if gotExempt != c.wantExempt {
			t.Errorf("%s: ExemptTransitProof(%q) = %v, want %v — %s",
				c.path, req.URL.Path, gotExempt, c.wantExempt, c.why)
		}
		if gotReached != c.wantReachedProt {
			t.Errorf("%s: reached a protected handler = %v, want %v (status %d) — %s",
				c.path, gotReached, c.wantReachedProt, rr.Code, c.why)
		}
		if gotExempt && gotReached {
			t.Errorf("⚠ %s is EXEMPT FROM THE GATEWAY SECRET AND REACHED A PROTECTED HANDLER. "+
				"That is an unauthenticated path to a guarded route. Do not relax this test — "+
				"the routing behaviour it depends on has changed and the exemption must be "+
				"re-derived.", c.path)
		}
		if c.wantReachedProt {
			sawControl = true
		}
	}
	if !sawControl {
		t.Fatal("no case in this table reaches a protected handler, so the table proves nothing")
	}
}
