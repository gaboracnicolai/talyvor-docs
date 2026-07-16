// Package pageindex throttles the async page-save semantic indexer — the largest
// uncontrolled Lens consumer in Docs.
//
// Before this, internal/page/store.go's Update spawned ONE goroutine PER SAVE, each firing a
// Lens embedding round-trip with no coalescing, no concurrency bound, and no rate limit (and
// the HTTP rate limiter can't see this internal path). A burst of edits — the frontend
// autosaves on a ~2s debounce — meant unbounded embed calls and unbounded goroutines.
//
// Throttle sits at the searchIndexer seam (it implements the same IndexPage(ctx, pageID,
// workspaceID, text) signature, so page.Store.WithIndexer accepts it) and delegates the
// actual embed to the real indexer, preserving the workspaceID argument unchanged — that arg
// is the seam a per-workspace metering label would ride, so the throttle must not drop it.
//
// Four guarantees:
//   - COALESCING (latest-wins): rapid saves to one page collapse to a single embed of the
//     final content.
//   - NEVER-DROP-FINAL: a save arriving during an in-flight embed is re-embedded afterwards —
//     the final saved state is always eventually embedded, never silently dropped. This holds
//     under coalescing AND under worker-pool backpressure.
//   - BOUNDED POOL: a fixed number of workers, so a burst across many pages can never spawn
//     more than pool-size concurrent embeds.
//   - RATE CAP: the total embed call rate is bounded (token bucket), independent of how Lens
//     meters — we don't flood it regardless.
//
// STALENESS is the coalescing window (and the config knob for max save→searchable delay under
// normal load): a page's first save schedules its embed staleness later; saves within the
// window update the pending content (coalesced) without extending the deadline, so the final
// state is embedded within ~staleness of the first save in a batch, plus any pool/rate wait.
package pageindex

import (
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RealIndexer is the embed sink the throttle delegates to — internal/search.SemanticSearch
// satisfies it. Same signature as page.searchIndexer.
type RealIndexer interface {
	IndexPage(ctx context.Context, pageID, workspaceID, text string) error
}

// Options configures the throttle. Zero values fall back to sane defaults (see New).
type Options struct {
	Workers      int           // concurrent embed workers (pool size)
	Staleness    time.Duration // coalescing window / target max save→searchable delay
	RatePerSec   float64       // embed call-rate ceiling; <=0 means unlimited
	Burst        int           // token-bucket burst
	EmbedTimeout time.Duration // per-embed context timeout
}

const (
	defaultWorkers      = 4
	defaultStaleness    = 5 * time.Second
	defaultBurst        = 10
	defaultEmbedTimeout = 30 * time.Second
)

// per-page lifecycle. A missing key reads as stIdle (the zero value), which is load-bearing.
const (
	stIdle = iota
	stScheduled
	stQueued
	stInflight
)

type entry struct{ ws, text string }

// Throttle coalesces + bounds + paces embeds for the per-save path.
type Throttle struct {
	real         RealIndexer
	staleness    time.Duration
	workers      int
	embedTimeout time.Duration
	limiter      *rate.Limiter // nil means unlimited

	mu       sync.Mutex
	cond     *sync.Cond
	latest   map[string]entry // pageID -> newest pending content (latest-wins)
	status   map[string]int   // pageID -> lifecycle
	queue    []string         // pageIDs ready for a worker (deduped by status)
	inflight int
	started  bool
	stopped  bool
	wg       sync.WaitGroup
}

// New builds a throttle. Call Start to spawn its workers.
func New(real RealIndexer, o Options) *Throttle {
	if o.Workers <= 0 {
		o.Workers = defaultWorkers
	}
	if o.Staleness <= 0 {
		o.Staleness = defaultStaleness
	}
	if o.EmbedTimeout <= 0 {
		o.EmbedTimeout = defaultEmbedTimeout
	}
	t := &Throttle{
		real:         real,
		staleness:    o.Staleness,
		workers:      o.Workers,
		embedTimeout: o.EmbedTimeout,
		latest:       map[string]entry{},
		status:       map[string]int{},
	}
	if o.RatePerSec > 0 {
		burst := o.Burst
		if burst <= 0 {
			burst = defaultBurst
		}
		t.limiter = rate.NewLimiter(rate.Limit(o.RatePerSec), burst)
	}
	t.cond = sync.NewCond(&t.mu)
	return t
}

// Start spawns the worker pool. Idempotent. If ctx is cancellable, the throttle stops when it
// is cancelled.
func (t *Throttle) Start(ctx context.Context) {
	t.mu.Lock()
	if t.started {
		t.mu.Unlock()
		return
	}
	t.started = true
	t.mu.Unlock()

	for i := 0; i < t.workers; i++ {
		t.wg.Add(1)
		go t.worker()
	}
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			t.Stop()
		}()
	}
}

// IndexPage enqueues pageID's latest content for embedding and returns IMMEDIATELY — it never
// blocks on Lens. It is the searchIndexer seam page.Store calls. The incoming ctx is only the
// caller's; the embed runs under its own timeout.
func (t *Throttle) IndexPage(_ context.Context, pageID, workspaceID, text string) error {
	t.mu.Lock()
	t.latest[pageID] = entry{ws: workspaceID, text: text}
	if t.status[pageID] == stIdle {
		// First save in a batch: schedule the embed one staleness window out. Later saves
		// within the window update `latest` above without rescheduling, so the deadline is
		// bounded to ~staleness from the first save (no starvation).
		t.status[pageID] = stScheduled
		t.mu.Unlock()
		time.AfterFunc(t.staleness, func() { t.promote(pageID) })
		return nil
	}
	// Already scheduled / queued / in-flight: the newer content is recorded in `latest`; the
	// existing schedule (or the post-embed re-queue) will pick it up. Never-drop.
	t.mu.Unlock()
	return nil
}

// promote moves a scheduled page onto the ready queue when its staleness window elapses.
func (t *Throttle) promote(pageID string) {
	t.mu.Lock()
	if t.status[pageID] == stScheduled {
		t.status[pageID] = stQueued
		t.queue = append(t.queue, pageID)
		t.cond.Signal()
	}
	t.mu.Unlock()
}

func (t *Throttle) worker() {
	defer t.wg.Done()
	for {
		t.mu.Lock()
		for len(t.queue) == 0 && !t.stopped {
			t.cond.Wait()
		}
		if t.stopped && len(t.queue) == 0 {
			t.mu.Unlock()
			return
		}
		pageID := t.queue[0]
		t.queue = t.queue[1:]
		e := t.latest[pageID]
		delete(t.latest, pageID) // snapshot the content to embed; a newer save re-adds it
		t.status[pageID] = stInflight
		t.inflight++
		t.mu.Unlock()

		t.embed(pageID, e)

		t.mu.Lock()
		t.inflight--
		if _, dirty := t.latest[pageID]; dirty {
			// A newer save arrived while we embedded → re-queue so the FINAL state is
			// embedded. Never-drop, including under pool backpressure.
			t.status[pageID] = stQueued
			t.queue = append(t.queue, pageID)
			t.cond.Signal()
		} else {
			delete(t.status, pageID) // back to idle (absent key == stIdle)
		}
		t.mu.Unlock()
	}
}

// embed performs one rate-limited embed via the real indexer.
func (t *Throttle) embed(pageID string, e entry) {
	ctx, cancel := context.WithTimeout(context.Background(), t.embedTimeout)
	defer cancel()
	if t.limiter != nil {
		// Pace the total embed call rate. Blocking here holds the worker, so the pool + the
		// limiter together bound both concurrency and rate.
		if err := t.limiter.Wait(ctx); err != nil {
			return // ctx timed out waiting for a token; the next save re-indexes
		}
	}
	_ = t.real.IndexPage(ctx, pageID, e.ws, e.text)
}

// Stats reports outstanding work: pending = scheduled or queued, inflight = currently
// embedding. Both zero means fully drained. Useful for shutdown draining, tests, and an
// operator gauge.
func (t *Throttle) Stats() (pending, inflight int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, st := range t.status {
		switch st {
		case stScheduled, stQueued:
			pending++
		case stInflight:
			inflight++
		}
	}
	return pending, t.inflight
}

// Stop signals the workers to drain the ready queue and exit, then blocks until they do.
func (t *Throttle) Stop() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	t.cond.Broadcast()
	t.mu.Unlock()
	t.wg.Wait()
}
