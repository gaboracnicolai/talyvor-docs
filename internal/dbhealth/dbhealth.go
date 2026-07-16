// Package dbhealth tracks whether Postgres is reachable and degrades the service cleanly
// when it is not.
//
// MEASURED BEHAVIOUR BEFORE THIS (not assumed — a throwaway probe drove a real request at a
// pool pointed to a closed port):
//
//	GET /v1/workspaces/{ws}/pages/stale  → 500 in ~0ms
//	GET /v1/spaces/{s}/pages/{p}         → 500 in ~0ms
//
// So Docs did NOT panic and did NOT hang: authz.Middleware queries workspace_members before
// any handler runs, that query fails, and it answers 500 AUTHZ_ERROR. The failure is fast
// because a refused connection fails fast; a blackholed network (packets dropped rather
// than refused) would instead block until pgx's connect timeout, which is where a hang
// would come from and why the short-circuit below matters.
//
// What was wrong was the SHAPE of the failure, not its existence:
//
//   - 500 vs 503. A 500 means "this server has a bug" — clients, proxies and caches treat
//     it as non-retryable and unhelpful. A 503 means "temporarily unable, try again", which
//     is exactly true during an outage and is what a client should back off on. Docs was
//     telling every caller the wrong thing about a recoverable condition.
//   - No readiness signal. /healthz is a hardcoded {"ok":true} that never touches the pool,
//     and there is no /readyz — so an orchestrator kept a replica that could not serve a
//     single request in the load-balancer rotation, forever.
//
// LIVENESS vs READINESS (a deliberate fork). /healthz stays DB-free and this package adds
// /readyz, which probes. That split is intentional: liveness answers "should this process be
// killed and restarted", and restarting a pod cannot fix a database outage — wiring the DB
// into /healthz would make a brief DB blip crash-loop every replica simultaneously, turning
// a recoverable incident into a self-inflicted outage. Readiness answers "should traffic be
// sent here", which is precisely what should go false when the DB is gone. The alternative
// (one DB-aware /healthz) is simpler and is what docker-compose's healthcheck already polls,
// but it is the dangerous one; see BUILD_STATE.
package dbhealth

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// probeTimeout bounds a single health probe. The probe must never become the outage: an
// unbounded Ping against a blackholed database would hang the readiness endpoint itself.
const probeTimeout = 2 * time.Second

// Pinger is the slice of *pgxpool.Pool this package needs. An interface so a test can
// supply a live pool, a dead one, or a stub without a database.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Checker caches the reachability of one pool.
type Checker struct {
	pool Pinger
	ttl  time.Duration

	mu        sync.Mutex
	healthy   bool
	checked   bool
	lastCheck time.Time

	probes atomic.Int64
}

// New returns a Checker for pool. ttl is how long a probe result is reused; 0 probes on
// every call (used by tests for determinism). Health is per-Checker state, never global —
// one instance going unhealthy must not affect another.
func New(pool Pinger, ttl time.Duration) *Checker {
	return &Checker{pool: pool, ttl: ttl}
}

// Healthy reports whether the database is reachable, reusing a recent probe.
//
// The cache is the point: without it every request carries a database round trip, and the
// health check becomes its own bottleneck — and, during an outage, every request would
// block on its own doomed connect attempt.
func (c *Checker) Healthy(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.checked && c.ttl > 0 && time.Since(c.lastCheck) < c.ttl {
		return c.healthy
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	c.probes.Add(1)
	err := c.pool.Ping(probeCtx)
	c.healthy = err == nil
	c.checked = true
	c.lastCheck = time.Now()
	return c.healthy
}

// Probes returns how many real pings have been issued — lets a test prove the cache works,
// and lets an operator see the probe rate.
func (c *Checker) Probes() int64 { return c.probes.Load() }

// ReadyHandler is the readiness probe: 200 when the database is reachable, 503 when it is
// not. Point an orchestrator's readinessProbe here so a replica that cannot serve is pulled
// from rotation instead of answering every request with an error.
func (c *Checker) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.Healthy(r.Context()) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok": false, "database": "unreachable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "database": "ok"})
	}
}

// Middleware short-circuits requests with a clean 503 while the database is unreachable.
//
// Two reasons this beats letting each handler fail on its own:
//   - It gives ONE honest, retryable answer. Otherwise the status depends on which query
//     happened to fail first — today that is authz's membership lookup answering 500, and
//     page.Get would answer 404 (telling every client and cache the page was DELETED).
//   - It fails FAST. Against a blackholed database each handler would otherwise block on its
//     own connect attempt, consuming a connection slot and the client's timeout budget.
//     The cached probe means one request pays that cost, not all of them.
//
// It is not a barrier: a database that dies between probes still lets requests through to
// fail on their own. That is the intended trade — the goal is graceful degradation, not a
// distributed transaction with the health checker.
func (c *Checker) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !c.Healthy(r.Context()) {
				w.Header().Set("Retry-After", "5")
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": "database temporarily unavailable, please retry",
					"code":  "DB_UNAVAILABLE",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
