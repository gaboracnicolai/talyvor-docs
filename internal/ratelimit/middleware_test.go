package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/ratelimit"
	"github.com/talyvor/docs/internal/testutil"
)

// SAME AUTHORITY DISCIPLINE AS #23, in a new place.
//
// The bucket key is a workspace id. On these routes it arrives as {wsID} in the URL —
// attacker-controlled. The AI handlers authorize it themselves, but middleware runs BEFORE
// the handler, so a limiter that keyed on the RAW param would be authority-blind and give
// an attacker two distinct wins:
//
//  1. EVASION — name a workspace you do not belong to (or a junk one) and spend from a
//     fresh bucket, forever, by rotating the string.
//  2. CROSS-TENANT DoS — hammer /workspaces/{victim}/ai/write to exhaust the VICTIM's
//     bucket and lock a tenant you cannot even read out of AI.
//
// So the middleware authorizes the workspace ITSELF and keys on the Membership it gets
// back — the same verified value, never the raw param. An unauthorized caller is rejected
// before a token is spent, which is what makes (2) impossible.

const rlSecret = "sec4-test-gateway-secret-0123456789"

// rlChain mirrors main.go: gatewayauth + authz, then the limiter, then a handler that
// records how many calls actually reached it.
func rlChain(d *testutil.DB, l *ratelimit.Limiter, reached *int) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(rlSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.With(l.WorkspaceLimit("wsID")).Post("/workspaces/{wsID}/ai/write",
			func(w http.ResponseWriter, _ *http.Request) {
				*reached++
				w.WriteHeader(http.StatusOK)
			})
	})
	return r
}

func rlReq(path, email string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"prompt":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", rlSecret)
	r.Header.Set("X-User-Email", email)
	return r
}

func TestWorkspaceLimit_PerTenantBurstAndIsolation(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")

	reached := 0
	l := ratelimit.New(60, 3) // 3 burst, ~1/s refill
	chain := rlChain(d, l, &reached)
	do := func(r *http.Request) int {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr.Code
	}

	// N in-window from one workspace → allowed.
	for i := 1; i <= 3; i++ {
		if code := do(rlReq("/v1/workspaces/"+wsA+"/ai/write", "alice@corp.com")); code != http.StatusOK {
			t.Fatalf("A call %d/3 = %d, want 200 (inside the burst)", i, code)
		}
	}
	// The (N+1)th → 429.
	if code := do(rlReq("/v1/workspaces/"+wsA+"/ai/write", "alice@corp.com")); code != http.StatusTooManyRequests {
		t.Errorf("A call 4 = %d, want 429 — the LLM endpoint is unthrottled and a single tenant can "+
			"drive unbounded Lens spend", code)
	}
	if reached != 3 {
		t.Errorf("%d calls reached the handler, want 3 — a throttled call must not execute (it would "+
			"still spend at Lens)", reached)
	}

	// PER-TENANT ISOLATION: B is untouched by A exhausting its bucket.
	for i := 1; i <= 3; i++ {
		if code := do(rlReq("/v1/workspaces/"+wsB+"/ai/write", "bob@corp.com")); code != http.StatusOK {
			t.Errorf("B call %d/3 = %d, want 200 — workspace A exhausting ITS bucket must not throttle "+
				"workspace B (that turns the limiter into a cross-tenant DoS)", i, code)
		}
	}
}

// EVASION: a caller must not be able to spend from a bucket they do not own by naming it.
func TestWorkspaceLimit_ForeignWorkspaceCannotDodgeOrExhaust(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")

	reached := 0
	l := ratelimit.New(60, 2)
	chain := rlChain(d, l, &reached)
	do := func(r *http.Request) int {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr.Code
	}

	// Alice exhausts her OWN bucket.
	for i := 0; i < 2; i++ {
		if code := do(rlReq("/v1/workspaces/"+wsA+"/ai/write", "alice@corp.com")); code != http.StatusOK {
			t.Fatalf("A warmup %d failed: %d", i, code)
		}
	}
	if code := do(rlReq("/v1/workspaces/"+wsA+"/ai/write", "alice@corp.com")); code != http.StatusTooManyRequests {
		t.Fatalf("A is not exhausted (%d) — fixture wrong", code)
	}

	// (a) EVASION — Alice names B's workspace to get a fresh bucket. Must be denied on
	// AUTHORITY (403), never served (200) and never merely throttled (429): a 429 here would
	// mean the limiter keyed on the raw param and she reached a foreign bucket at all.
	code := do(rlReq("/v1/workspaces/"+wsB+"/ai/write", "alice@corp.com"))
	if code == http.StatusOK {
		t.Errorf("Alice spent from workspace B's bucket by naming it in the URL (%d) — the limiter "+
			"keyed on an UNVERIFIED param, so anyone evades the limit by rotating the workspace id", code)
	}
	if code != http.StatusForbidden {
		t.Errorf("Alice→B = %d, want 403 (authority denial before a token is spent)", code)
	}

	// (b) A junk workspace id must not mint a fresh bucket either.
	if code := do(rlReq("/v1/workspaces/ws-does-not-exist/ai/write", "alice@corp.com")); code != http.StatusForbidden {
		t.Errorf("junk workspace id = %d, want 403 — a caller must not be able to rotate the key to "+
			"an unlimited supply of fresh buckets", code)
	}

	// (c) CROSS-TENANT DoS — Alice's rejected attempts must not have consumed B's tokens.
	// Bob's full burst must still be available.
	for i := 1; i <= 2; i++ {
		if code := do(rlReq("/v1/workspaces/"+wsB+"/ai/write", "bob@corp.com")); code != http.StatusOK {
			t.Errorf("Bob call %d/2 = %d, want 200 — Alice's DENIED requests still drained B's bucket, "+
				"so an outsider can lock a tenant out of AI by hammering their workspace id", i, code)
		}
	}

	if reached != 4 {
		t.Errorf("handler reached %d times, want 4 (2 from A + 2 from B); no denied call may execute", reached)
	}
}

// A caller with a valid transit proof but ZERO memberships must not reach the LLM at all.
func TestWorkspaceLimit_ZeroMembershipCallerDenied(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	d.Member(t, ws, "bob@corp.com")

	reached := 0
	l := ratelimit.New(60, 5)
	chain := rlChain(d, l, &reached)

	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, rlReq("/v1/workspaces/"+ws+"/ai/write", "nobody@corp.com"))
	if rr.Code != http.StatusForbidden {
		t.Errorf("zero-membership caller = %d, want 403", rr.Code)
	}
	if reached != 0 {
		t.Errorf("handler reached %d times by a zero-membership caller, want 0", reached)
	}
}

// The 429 must be actionable: a client (and the frontend) needs to know when to retry.
func TestWorkspaceLimit_429CarriesRetryAfter(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	d.Member(t, ws, "alice@corp.com")

	reached := 0
	l := ratelimit.New(60, 1)
	chain := rlChain(d, l, &reached)

	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, rlReq("/v1/workspaces/"+ws+"/ai/write", "alice@corp.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("warmup = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, rlReq("/v1/workspaces/"+ws+"/ai/write", "alice@corp.com"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second call = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("429 has no Retry-After header — a client cannot tell a rate limit from a hard failure")
	}
}
