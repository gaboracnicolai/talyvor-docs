package pageindex

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReal is a stand-in for the real semantic indexer. It records every embed call (page +
// content + time), can block on a gate to simulate a slow Lens round-trip, and tracks
// concurrency so the pool bound is observable.
type fakeReal struct {
	mu    sync.Mutex
	calls []recorded
	gate  chan struct{} // if non-nil, each call blocks until it can receive
	conc  int32
	maxC  int32
}

type recorded struct {
	pageID, ws, text string
	at               time.Time
}

func (f *fakeReal) IndexPage(_ context.Context, pageID, ws, text string) error {
	c := atomic.AddInt32(&f.conc, 1)
	for {
		m := atomic.LoadInt32(&f.maxC)
		if c <= m || atomic.CompareAndSwapInt32(&f.maxC, m, c) {
			break
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.calls = append(f.calls, recorded{pageID, ws, text, time.Now()})
	f.mu.Unlock()
	atomic.AddInt32(&f.conc, -1)
	return nil
}

func (f *fakeReal) snapshot() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.calls...)
}
func (f *fakeReal) count() int { return len(f.snapshot()) }
func (f *fakeReal) lastFor(pageID string) (string, bool) {
	var out string
	var ok bool
	for _, c := range f.snapshot() {
		if c.pageID == pageID {
			out, ok = c.text, true
		}
	}
	return out, ok
}

// waitIdle blocks until the throttle has drained (no pending, no inflight) or times out.
func waitIdle(t *testing.T, th *Throttle, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p, in := th.Stats()
		if p == 0 && in == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	p, in := th.Stats()
	t.Fatalf("throttle did not drain within %v (pending=%d inflight=%d)", d, p, in)
}

func newTestThrottle(real RealIndexer, o Options) *Throttle {
	th := New(real, o)
	th.Start(context.Background())
	return th
}

// PROPERTY 1a — COALESCING (latest-wins): many rapid saves to ONE page collapse to a single
// embed carrying the FINAL content, not a stale intermediate.
func TestCoalescing_RapidSavesCollapseToOneEmbedOfFinalContent(t *testing.T) {
	real := &fakeReal{}
	th := newTestThrottle(real, Options{Workers: 2, Staleness: 40 * time.Millisecond, RatePerSec: 1000, Burst: 100})
	defer th.Stop()

	for i := 0; i < 10; i++ {
		_ = th.IndexPage(context.Background(), "page-1", "ws-1", "content-"+string(rune('a'+i)))
	}
	waitIdle(t, th, 2*time.Second)

	if n := real.count(); n != 1 {
		t.Errorf("10 rapid saves to one page produced %d embeds, want exactly 1 (coalesced)", n)
	}
	if last, _ := real.lastFor("page-1"); last != "content-j" {
		t.Errorf("embedded %q, want the FINAL content %q — a stale intermediate was embedded", last, "content-j")
	}
}

// PROPERTY 1b — NEVER-DROP-FINAL: a save that arrives WHILE an embed is in-flight must still
// result in a follow-up embed of the newer content. The final saved state is always embedded.
func TestNeverDrop_SaveDuringInflightIsReembedded(t *testing.T) {
	gate := make(chan struct{})
	real := &fakeReal{gate: gate}
	th := newTestThrottle(real, Options{Workers: 1, Staleness: 10 * time.Millisecond, RatePerSec: 1000, Burst: 100})
	defer th.Stop()

	// First save → becomes in-flight (blocked on the gate).
	_ = th.IndexPage(context.Background(), "page-1", "ws-1", "v1")
	waitInflight(t, th, 1, time.Second)

	// A newer save arrives WHILE v1 is embedding.
	_ = th.IndexPage(context.Background(), "page-1", "ws-1", "v2")

	// Release the first embed; the throttle must notice the newer content and re-embed it.
	close(gate)
	waitIdle(t, th, 2*time.Second)

	calls := real.snapshot()
	if len(calls) != 2 {
		t.Fatalf("got %d embeds, want 2 (v1 then a follow-up of v2) — the newer save was dropped", len(calls))
	}
	if last, _ := real.lastFor("page-1"); last != "v2" {
		t.Errorf("final embed was %q, want %q — the newer content saved during the in-flight embed was LOST", last, "v2")
	}
}

// The workspace label (the arg the real embed seam reads) must ride through unchanged — the
// throttle coalesces content but must not corrupt the workspace it is attributed to.
func TestWorkspaceArgIsPreserved(t *testing.T) {
	real := &fakeReal{}
	th := newTestThrottle(real, Options{Workers: 2, Staleness: 20 * time.Millisecond, RatePerSec: 1000, Burst: 100})
	defer th.Stop()

	_ = th.IndexPage(context.Background(), "page-1", "ws-alpha", "hello")
	waitIdle(t, th, 2*time.Second)

	calls := real.snapshot()
	if len(calls) != 1 || calls[0].ws != "ws-alpha" {
		t.Errorf("embed carried ws=%q, want ws-alpha — the workspace label the metering seam depends on was dropped/altered", func() string {
			if len(calls) == 1 {
				return calls[0].ws
			}
			return "<no single call>"
		}())
	}
}

func waitInflight(t *testing.T, th *Throttle, want int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, in := th.Stats(); in == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, in := th.Stats()
	t.Fatalf("wanted inflight=%d within %v, got %d", want, d, in)
}
