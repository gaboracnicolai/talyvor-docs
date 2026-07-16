package pageindex

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// PROPERTY 2 — BOUNDED POOL: a burst across MANY distinct pages can never spawn more than
// pool-size concurrent embeds; the rest queue, and all eventually embed. No
// goroutine-per-save embed explosion.
func TestBoundedPool_ConcurrencyNeverExceedsPoolSize(t *testing.T) {
	const pool = 3
	const pages = 20
	gate := make(chan struct{})
	real := &fakeReal{gate: gate}
	th := newTestThrottle(real, Options{Workers: pool, Staleness: 10 * time.Millisecond, RatePerSec: 100000, Burst: 1000})
	defer th.Stop()

	for i := 0; i < pages; i++ {
		_ = th.IndexPage(context.Background(), fmt.Sprintf("page-%d", i), "ws-1", "content")
	}
	// Let the workers saturate against the gate, then hold a moment to catch any over-spawn.
	waitInflight(t, th, pool, 2*time.Second)
	time.Sleep(80 * time.Millisecond)

	if mc := atomic.LoadInt32(&real.maxC); mc > pool {
		t.Errorf("observed %d concurrent embeds, want <= pool size %d — the pool is not bounding concurrency", mc, pool)
	}

	close(gate) // release all
	waitIdle(t, th, 3*time.Second)

	if n := real.count(); n != pages {
		t.Errorf("embedded %d of %d distinct pages — some were dropped, not just delayed", n, pages)
	}
	if mc := atomic.LoadInt32(&real.maxC); mc > pool {
		t.Errorf("peak concurrency %d exceeded pool size %d at some point", mc, pool)
	}
}

// PROPERTY 3 — RATE CAP: driving embeds faster than the ceiling paces them to the limit
// rather than bursting. Consumer-side defense, independent of how Lens meters.
func TestRateCap_EmbedsArePacedToTheCeiling(t *testing.T) {
	const perSec = 20.0 // one token every 50ms
	const n = 6
	real := &fakeReal{}
	// Plenty of workers so the pool is NOT the bottleneck — the rate limiter is what paces.
	th := newTestThrottle(real, Options{Workers: 8, Staleness: 5 * time.Millisecond, RatePerSec: perSec, Burst: 1})
	defer th.Stop()

	start := time.Now()
	for i := 0; i < n; i++ {
		_ = th.IndexPage(context.Background(), fmt.Sprintf("page-%d", i), "ws-1", "content")
	}
	waitIdle(t, th, 3*time.Second)

	calls := real.snapshot()
	if len(calls) != n {
		t.Fatalf("got %d embeds, want %d", len(calls), n)
	}
	// n tokens at 1/50ms, burst 1 → the run must span at least (n-1)*50ms. Without pacing all
	// n complete in a few ms. Use a lenient floor to avoid CI flakiness.
	elapsed := calls[n-1].at.Sub(start)
	floor := time.Duration(float64(n-1)/perSec*float64(time.Second)) * 8 / 10 // 80% of ideal
	if elapsed < floor {
		t.Errorf("%d embeds completed in %v — faster than the %v/s ceiling allows (>= ~%v); not paced",
			n, elapsed, perSec, floor)
	}
}

// PROPERTY 4 — STALENESS: the final state is embedded within ~the staleness budget under
// normal load (no backlog). Not instant (it coalesces), not never.
func TestStaleness_FinalStateEmbeddedWithinBudget(t *testing.T) {
	const staleness = 60 * time.Millisecond
	real := &fakeReal{}
	th := newTestThrottle(real, Options{Workers: 2, Staleness: staleness, RatePerSec: 100000, Burst: 100})
	defer th.Stop()

	start := time.Now()
	_ = th.IndexPage(context.Background(), "page-1", "ws-1", "hello")
	waitIdle(t, th, 2*time.Second)

	calls := real.snapshot()
	if len(calls) != 1 {
		t.Fatalf("got %d embeds, want 1", len(calls))
	}
	delay := calls[0].at.Sub(start)
	if delay < staleness*7/10 {
		t.Errorf("embedded after %v — sooner than the %v coalescing window (didn't coalesce)", delay, staleness)
	}
	if delay > staleness+400*time.Millisecond {
		t.Errorf("embedded after %v — far beyond the %v staleness budget under no load", delay, staleness)
	}
}

func TestDefaults_AreSaneAndOverridable(t *testing.T) {
	d := New(&fakeReal{}, Options{}) // all zero → defaults
	if d.workers != defaultWorkers || d.staleness != defaultStaleness || d.embedTimeout != defaultEmbedTimeout {
		t.Errorf("defaults wrong: workers=%d staleness=%v embedTimeout=%v", d.workers, d.staleness, d.embedTimeout)
	}
	if d.limiter != nil {
		t.Error("no RatePerSec given → limiter should be nil (unlimited), got a limiter")
	}
	o := New(&fakeReal{}, Options{Workers: 7, Staleness: 2 * time.Second, RatePerSec: 3, Burst: 5})
	if o.workers != 7 || o.staleness != 2*time.Second || o.limiter == nil {
		t.Errorf("overrides not honored: workers=%d staleness=%v limiter=%v", o.workers, o.staleness, o.limiter)
	}
}

// NEVER-DROP UNDER WORKER-POOL BACKPRESSURE: with the single worker blocked and a queue
// behind it, a newer save for the in-flight page AND fresh pages must all reach their FINAL
// embedded state once the pool drains. This is the guard's "prove never-drop under
// backpressure" case.
func TestNeverDrop_UnderPoolBackpressure(t *testing.T) {
	gate := make(chan struct{})
	real := &fakeReal{gate: gate}
	th := newTestThrottle(real, Options{Workers: 1, Staleness: 10 * time.Millisecond, RatePerSec: 100000, Burst: 100})
	defer th.Stop()

	_ = th.IndexPage(context.Background(), "A", "ws", "A-v1")
	waitInflight(t, th, 1, time.Second) // A-v1 blocked in the single worker

	// Backpressure: a newer A, plus B and C stacked behind the blocked worker.
	_ = th.IndexPage(context.Background(), "A", "ws", "A-v2")
	_ = th.IndexPage(context.Background(), "B", "ws", "B-v1")
	_ = th.IndexPage(context.Background(), "C", "ws", "C-v1")

	close(gate)
	waitIdle(t, th, 3*time.Second)

	for _, tc := range []struct{ page, want string }{{"A", "A-v2"}, {"B", "B-v1"}, {"C", "C-v1"}} {
		got, ok := real.lastFor(tc.page)
		if !ok {
			t.Errorf("page %s was never embedded — dropped under backpressure", tc.page)
		} else if got != tc.want {
			t.Errorf("page %s final embed = %q, want %q", tc.page, got, tc.want)
		}
	}
	// A must have embedded exactly twice (v1 then v2), never more.
	var aCount int
	for _, c := range real.snapshot() {
		if c.pageID == "A" {
			aCount++
		}
	}
	if aCount != 2 {
		t.Errorf("page A embedded %d times, want 2 (v1 + the re-embed of v2)", aCount)
	}
}
