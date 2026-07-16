package dbhealth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/dbhealth"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// What Docs does when Postgres is gone was never measured — BUILD_STATE recorded that
// /healthz is a hardcoded {"ok":true} that never touches the pool and that there is no
// /readyz, but not what a REQUEST does. These tests measure it and then pin the contract:
// a clean 503, never a panic, never a hang.

const outSecret = "sec4-test-gateway-secret-0123456789"

// deadPool returns a pool whose database is unreachable: a real, well-formed pool pointed
// at a closed port. This is a truer outage than pool.Close() — a closed pool fails
// instantly with "closed pool", whereas a real outage makes pgx try to DIAL, which is where
// a hang would come from.
func deadPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://nobody:nobody@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("parse dead dsn: %v", err)
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open dead pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func outReq(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Gateway-Auth", outSecret)
	r.Header.Set("X-User-Email", "alice@corp.com")
	return r
}

// The readiness probe must reflect the DB. Without it an orchestrator keeps a replica that
// cannot serve a single request in the load-balancer rotation, and /healthz — which is a
// literal — will never tell it otherwise.
func TestReadyz_ReflectsDBHealth(t *testing.T) {
	d := testutil.New(t) // live pool

	live := dbhealth.New(d.Pool, 0) // ttl 0 = probe every call, deterministic in tests
	rr := httptest.NewRecorder()
	live.ReadyHandler()(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("/readyz with a LIVE database = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	dead := dbhealth.New(deadPool(t), 0)
	rr = httptest.NewRecorder()
	dead.ReadyHandler()(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with an UNREACHABLE database = %d, want 503 — the probe must reflect DB "+
			"health or a broken replica stays in rotation. body=%s", rr.Code, rr.Body.String())
	}
}

// The core contract: a request during an outage gets a clean 503. Not a panic, not a hang,
// not a 404 (page.Get returns 404 on ANY error, which tells a client and every cache in
// between that the page was DELETED).
func TestOutage_HandlerReturns503NotPanicNotHang(t *testing.T) {
	dead := deadPool(t)
	checker := dbhealth.New(dead, 0)

	pageHandler := page.NewHandler(page.NewStore(dead), dead)
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(outSecret, exempt))
		r.Use(checker.Middleware()) // short-circuits to 503 while the DB is unreachable
		r.Use(authz.Middleware(authz.NewPGResolver(dead), exempt))
		pageHandler.Mount(r)
	})

	done := make(chan int, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- -1 // panic
			}
		}()
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, outReq("/v1/workspaces/ws-1/pages/stale"))
		done <- rr.Code
	}()

	select {
	case code := <-done:
		if code == -1 {
			t.Fatal("handler PANICKED during a database outage — middleware.Recoverer turning that " +
				"into a 500 is not a defence worth relying on")
		}
		if code != http.StatusServiceUnavailable {
			t.Errorf("request during a DB outage = %d, want 503. A 5xx that is not 503 tells clients "+
				"nothing about retrying; a 404 would actively lie.", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request HUNG for 10s during a database outage — a hang is worse than an error: it " +
			"consumes a connection slot and the client's timeout budget")
	}
}

// Degradation must be temporary: when the DB returns, so does the service. A checker that
// latches unhealthy would turn a blip into an outage needing a restart.
func TestOutage_RecoversWhenDBReturns(t *testing.T) {
	d := testutil.New(t)
	checker := dbhealth.New(d.Pool, 0)

	if !checker.Healthy(context.Background()) {
		t.Fatal("checker reports a live database unhealthy")
	}
	// Simulate the outage window by pointing the checker at a dead pool, then back.
	dead := dbhealth.New(deadPool(t), 0)
	if dead.Healthy(context.Background()) {
		t.Fatal("checker reports an unreachable database healthy — it is not probing at all")
	}
	// The live checker must still be fine (no shared latch / global state).
	if !checker.Healthy(context.Background()) {
		t.Error("a live checker went unhealthy because a DIFFERENT checker saw an outage — health " +
			"must not be global mutable state")
	}
}

// The probe must not become its own outage: a cached result means one slow ping cannot
// serialise every request behind it.
func TestHealthy_CachesWithinTTL(t *testing.T) {
	d := testutil.New(t)
	c := dbhealth.New(d.Pool, time.Minute)

	if !c.Healthy(context.Background()) {
		t.Fatal("first probe says unhealthy")
	}
	before := c.Probes()
	for i := 0; i < 50; i++ {
		c.Healthy(context.Background())
	}
	if got := c.Probes() - before; got != 0 {
		t.Errorf("%d extra probes inside the TTL, want 0 — an uncached probe puts a database round "+
			"trip on every request, which is a self-inflicted bottleneck", got)
	}
}
