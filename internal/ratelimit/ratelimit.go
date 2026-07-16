// Package ratelimit is a per-key token-bucket rate limiter, used to bound the rate at
// which a single workspace can drive Docs's LLM endpoints.
//
// WHY THIS EXISTS (and what it is NOT). Docs calls Lens with ONE service API key for the
// whole instance and labels the workspace in an X-Talyvor-Workspace header; `ai.Engine.run`
// gates only on "is Lens configured". There is no balance check, no quota and no cost cap
// anywhere in this repository — `pages.ai_cost_usd` is a report synced back from Track,
// never read to decide. So this limiter is the ONLY per-tenant LLM control Docs has.
//
// It bounds RATE, not COST. It is a burst/abuse ceiling, not a billing system: it cannot
// know that one request cost 100x another. If Lens meters per workspace, this is
// defence-in-depth on top of that; if Lens meters per API key, this is also what stops one
// tenant exhausting the whole instance's budget and taking AI down for everyone else. A
// real per-tenant budget cap is a separate, larger piece of work.
//
// DESIGN FORKS (documented; alternatives in BUILD_STATE):
//
//   - TOKEN BUCKET, not fixed window. A fixed window admits 2N calls across a boundary —
//     N at the end of one window and N at the start of the next — which is precisely the
//     burst this exists to stop. A bucket also fits the usage shape: a writer fires a few
//     AI calls then idles, and should not be charged for idling. Implemented over
//     golang.org/x/time/rate rather than hand-rolled, because bucket arithmetic under
//     concurrency is easy to get subtly wrong and that package is the canonical, tested one.
//
//   - PER-WORKSPACE key, not per-member-per-workspace. Cost is a tenant-level concern: a
//     ten-person workspace keyed per member would spend ten times the intended ceiling. The
//     trade-off is that one heavy user can consume their colleagues' allowance — visible and
//     recoverable, unlike a surprise bill. Per-member is a one-line change to the key if the
//     product wants it later.
//
//   - IN-MEMORY, not shared. Docs is single-replica today (BUILD_STATE §3), so an in-process
//     map is correct and adds no dependency. With N replicas the effective ceiling becomes
//     N× the configured one — degrading toward the current unlimited state rather than
//     wrongly denying, which is the safer direction to be wrong in. A shared store (Redis)
//     becomes necessary the day HA lands; that is called out in BUILD_STATE.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultTTL is how long an idle bucket is kept before eviction. Long enough that a normal
// editing session keeps its bucket (so the ceiling is real), short enough that the map
// tracks live tenants rather than every workspace ever seen.
const defaultTTL = 10 * time.Minute

// sweepEvery bounds how often a sweep runs, so Allow stays O(1) amortised rather than
// walking the map on every call.
const sweepEvery = time.Minute

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// Limiter is a per-key token bucket. Safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	perSec float64
	burst  int
	ttl    time.Duration

	lastSweep time.Time
	// now is injectable so eviction is testable without sleeping in production code.
	now func() time.Time
}

// New returns a limiter admitting perMinute requests per key per minute, with burst
// capacity for a short spike.
//
// FAIL CLOSED: a non-positive rate or burst denies everything rather than becoming an
// accidental no-op. A misconfigured limiter that silently allows all traffic is the
// fail-open shape this codebase has been burned by; better to be conspicuously broken.
func New(perMinute float64, burst int) *Limiter {
	return &Limiter{
		buckets:   map[string]*bucket{},
		perSec:    perMinute / 60,
		burst:     burst,
		ttl:       defaultTTL,
		lastSweep: time.Now(),
		now:       time.Now,
	}
}

// WithTTL overrides how long an idle bucket survives. Chainable.
func (l *Limiter) WithTTL(d time.Duration) *Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ttl = d
	return l
}

// Enabled reports whether this limiter admits anything at all. A limiter built with a
// non-positive rate/burst is disabled-by-failing-closed, which callers may want to surface
// at boot rather than discover as a wall of 429s.
func (l *Limiter) Enabled() bool { return l.perSec > 0 && l.burst > 0 }

// Allow consumes one token for key and reports whether the call may proceed.
func (l *Limiter) Allow(key string) bool {
	if !l.Enabled() {
		return false // fail closed — see New
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Limit(l.perSec), l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.Allow()
}

// Buckets returns the number of live buckets — for tests and for an operator asking how
// many tenants are currently being tracked.
func (l *Limiter) Buckets() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// sweepLocked drops buckets idle for longer than the TTL. Rate-limited to sweepEvery so
// Allow stays cheap. Caller must hold l.mu.
//
// An evicted key is simply re-created full on its next call. That is deliberate: it can
// only ever be MORE permissive than keeping the bucket, never less, and the TTL is far
// longer than the refill window — so a key idle long enough to be evicted would have
// refilled to full anyway. Eviction therefore costs no enforcement.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery && len(l.buckets) > 0 && l.ttl >= sweepEvery {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.seen) > l.ttl {
			delete(l.buckets, k)
		}
	}
}
