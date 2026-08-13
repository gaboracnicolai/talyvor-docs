package dbhealth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/dbhealth"
	"github.com/talyvor/docs/internal/testutil"
)

// THE PROBE RESULT IS SHARED STATE AND IT WAS DERIVED FROM ONE CALLER'S LIFETIME.
//
// Checker.Healthy cached what `pool.Ping(probeCtx)` said, and probeCtx was derived from the
// context of whichever request happened to find the cache stale. A caller whose context was
// already done — a browser that navigated away, a proxy that gave up, an orchestrator probe
// whose client-side timeout is shorter than the ping — made Ping return that caller's
// context error, which was cached as `healthy = false` FOR EVERY OTHER CALLER for the whole
// TTL (5s in cmd/docs/main.go). MEASURED on real Postgres, database up throughout:
//
//	Healthy(cancelled caller ctx) = false        probes = 1
//	  next caller, context.Background()  = false probes = 1   ← the cache, not a probe
//	  next caller, context.Background()  = false probes = 1
//	Middleware, unrelated healthy request  → 503 {"code":"DB_UNAVAILABLE"}
//	/readyz                                → 503 {"database":"unreachable"}
//	SELECT 1 over the same pool            → 1   (the database was up the entire time)
//
// The blast radius is the whole replica: Middleware() fronts every /v1 and /mcp route
// (cmd/docs/main.go), so one client hanging up 503s everybody, and /readyz simultaneously
// tells the orchestrator to pull the replica out of rotation. The readiness probe is itself
// a caller, so the shape is self-amplifying: a database slow enough to outlast kubelet's
// timeoutSeconds turns "one slow ping" into "this replica serves nothing for 5s", and the
// next probe repeats it.
//
// ⚠ WHY THE EXISTING SUITE COULD NOT SEE IT. outage_test.go builds every Checker with
// `dbhealth.New(pool, 0)` — "ttl 0 = probe every call, deterministic in tests". With ttl 0
// the cache branch is unreachable BY CONSTRUCTION, so the tests that prove the outage
// contract cannot execute the line that carries this defect. Production is the only
// configuration with a non-zero ttl. Every Checker below is built with the PRODUCTION ttl.
//
// ⚠ WHAT IS DELIBERATELY NOT ASSERTED: that a caller with a dead context gets an error.
// "Is the database reachable" is not a question about the caller, and that caller's own
// handler will fail on its own dead context a moment later regardless. The contract fixed
// here is only that one caller's lifetime must not decide a shared fact.
const prodTTL = 5 * time.Second // cmd/docs/main.go:544

// deadCallerCtx is a caller whose context is already done — the ordinary client hang-up.
func deadCallerCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestHealthy_ACancelledCallerDoesNotPoisonTheSharedCache_RealPG(t *testing.T) {
	d := testutil.New(t) // live pool onto a real Postgres
	c := dbhealth.New(d.Pool, prodTTL)

	before := c.Probes()
	c.Healthy(deadCallerCtx())

	// VACUITY FLOOR, and it is stated as one rather than claimed as a defect probe: this
	// passes at baseline. It exists so a later "fix" that simply skips the probe when the
	// caller is done — handing back a stale cached verdict that is never refreshed — cannot
	// satisfy the assertions below by never asking the database anything.
	if got := c.Probes() - before; got != 1 {
		t.Errorf("[PROBE-RAN] the cancelled caller issued %d probes, want 1 — Healthy must still "+
			"ask the database; returning a stale cached verdict is a different defect", got)
	}

	// THE FINDING. An unrelated caller, with a perfectly good context, immediately after.
	if !c.Healthy(context.Background()) {
		t.Errorf("[SHARED-CACHE] a caller with a live context was told the database is unreachable " +
			"because a DIFFERENT caller's context was cancelled during the probe — the database is up")
	}

	// The same thing through the middleware that fronts every /v1 and /mcp route.
	h := c.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`served`))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/spaces/s/pages/p", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("[MIDDLEWARE] an unrelated request got %d %s — one client hanging up must not "+
			"503 every other request for the TTL", rr.Code, rr.Body.String())
	}

	// And what the orchestrator is told.
	rr2 := httptest.NewRecorder()
	c.ReadyHandler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr2.Code != http.StatusOK {
		t.Errorf("[READYZ] readiness reported %d %s after one cancelled caller — an orchestrator "+
			"pulls a healthy replica out of rotation on this", rr2.Code, rr2.Body.String())
	}

	// Proof the premise held for the whole test: the database really was up.
	var one int
	if err := d.Pool.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("premise broken — the database was NOT reachable during this test: %v", err)
	}
}

// The other everyday shape of the same trigger: a caller whose DEADLINE expired rather than
// one that was explicitly cancelled. This is the orchestrator-probe case (kubelet's default
// timeoutSeconds is 1) and the proxy-gave-up case, and it reaches the same line by a
// different door — so it is asserted separately rather than assumed to follow.
func TestHealthy_AnExpiredCallerDeadlineDoesNotPoisonTheSharedCache_RealPG(t *testing.T) {
	d := testutil.New(t)
	c := dbhealth.New(d.Pool, prodTTL)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	c.Healthy(expired)

	if !c.Healthy(context.Background()) {
		t.Errorf("[CALLER-DEADLINE] an expired caller deadline was cached as \"the database is " +
			"unreachable\" for every subsequent caller — the database is up")
	}
}

// ⚠⚠ THE CHECK THAT KEEPS THE CHECKER FALSIFIABLE. Detaching the probe from the caller's
// cancellation is one edit away from "the probe can no longer fail", and a health check that
// cannot go red is worse than none — it is the shape this repository has already shipped
// once. A genuinely unreachable database must still be reported unreachable, through the
// same Checker, at the same production TTL, and it must still be CACHED as such.
func TestHealthy_AGenuineOutageIsStillReported(t *testing.T) {
	c := dbhealth.New(deadPool(t), prodTTL)

	if c.Healthy(context.Background()) {
		t.Fatalf("[REAL-OUTAGE] a pool pointed at a closed port reported HEALTHY — the probe " +
			"cannot fail, so nothing below it means anything")
	}
	if c.Healthy(context.Background()) {
		t.Errorf("[REAL-OUTAGE] the second call reported healthy — an outage verdict must be " +
			"cached like any other, or the TTL is doing nothing during the incident")
	}
	rr := httptest.NewRecorder()
	c.ReadyHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("[REAL-OUTAGE] readiness answered %d during a real outage, want 503", rr.Code)
	}
}
