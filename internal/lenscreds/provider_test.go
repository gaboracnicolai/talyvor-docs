package lenscreds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a deterministic, advanceable clock shared by the provider (its Now) and the
// mint server (which stamps expires_at). Advancing it drives refresh-before-expiry.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// mintServer stands in for Lens's POST /v1/auth/token. It records every mint (workspace +
// the admin key it was called with) and returns a distinct token each time so a re-mint is
// observable. expires_at is stamped from the shared clock so the provider's refresh math is
// deterministic.
type mintServer struct {
	*httptest.Server
	clock *fakeClock

	mu        sync.Mutex
	mints     int32
	byWS      map[string]int // per-workspace mint count
	lastAdmin string
	gate      chan struct{} // if non-nil, each mint blocks until released (concurrency test)
}

func newMintServer(t *testing.T, clock *fakeClock) *mintServer {
	t.Helper()
	m := &mintServer{clock: clock, byWS: map[string]int{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/auth/token" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		var body struct {
			WorkspaceID string `json:"workspace_id"`
			TTLHours    int    `json:"ttl_hours"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if m.gate != nil {
			<-m.gate
		}
		m.mu.Lock()
		n := atomic.AddInt32(&m.mints, 1)
		m.byWS[body.WorkspaceID]++
		m.lastAdmin = r.Header.Get("Authorization")
		m.mu.Unlock()
		if body.TTLHours <= 0 {
			http.Error(w, "missing ttl_hours", http.StatusBadRequest)
			return
		}
		exp := clock.now().Add(time.Duration(body.TTLHours) * time.Hour)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("tok-%s-%d", body.WorkspaceID, n),
			"expires_at": exp.Format(time.RFC3339),
		})
	}))
	return m
}

func (m *mintServer) count() int32 {
	return atomic.LoadInt32(&m.mints)
}
func (m *mintServer) wsCount(ws string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byWS[ws]
}

// PROPERTY 1a — MINT WITH THE ADMIN KEY: TokenFor mints a per-workspace token via
// POST /v1/auth/token, sending the ADMIN key as the bearer, and returns the minted token.
func TestTokenFor_MintsWithAdminKey(t *testing.T) {
	clock := newClock()
	srv := newMintServer(t, clock)
	defer srv.Close()
	p := New(srv.URL, "admin-key", Options{TTL: time.Hour, Skew: 5 * time.Minute, Now: clock.now, HTTP: srv.Client()})

	tok, err := p.TokenFor(context.Background(), "wsA")
	if err != nil {
		t.Fatalf("TokenFor: %v", err)
	}
	if tok != "tok-wsA-1" {
		t.Fatalf("got token %q, want the minted tok-wsA-1", tok)
	}
	if srv.count() != 1 {
		t.Fatalf("mint count = %d, want 1", srv.count())
	}
	if srv.lastAdmin != "Bearer admin-key" {
		t.Fatalf("mint called with Authorization %q, want 'Bearer admin-key' (the admin key mints)", srv.lastAdmin)
	}
}

// PROPERTY 1b — CACHE (no re-mint): a second TokenFor for the same workspace, well before
// expiry, returns the cached token WITHOUT minting again. Minting per call would defeat the
// page-save throttle.
func TestTokenFor_CachesAndDoesNotRemint(t *testing.T) {
	clock := newClock()
	srv := newMintServer(t, clock)
	defer srv.Close()
	p := New(srv.URL, "admin-key", Options{TTL: time.Hour, Skew: 5 * time.Minute, Now: clock.now, HTTP: srv.Client()})

	t1, _ := p.TokenFor(context.Background(), "wsA")
	t2, _ := p.TokenFor(context.Background(), "wsA")
	if srv.count() != 1 {
		t.Fatalf("mint count = %d after two calls, want 1 (second call must hit cache)", srv.count())
	}
	if t1 != t2 {
		t.Fatalf("cached token changed: %q then %q", t1, t2)
	}
}

// PROPERTY 1c — REFRESH BEFORE EXPIRY: once the clock is within `skew` of expiry, TokenFor
// mints a fresh token instead of returning the stale one.
func TestTokenFor_RefreshesBeforeExpiry(t *testing.T) {
	clock := newClock()
	srv := newMintServer(t, clock)
	defer srv.Close()
	p := New(srv.URL, "admin-key", Options{TTL: time.Hour, Skew: 5 * time.Minute, Now: clock.now, HTTP: srv.Client()})

	t1, _ := p.TokenFor(context.Background(), "wsA") // expires at base+1h
	// Still comfortably inside the window → cache hit.
	clock.advance(50 * time.Minute)
	if _, _ = p.TokenFor(context.Background(), "wsA"); srv.count() != 1 {
		t.Fatalf("re-minted at 50m (expiry 60m, skew 5m) — should still be cached; mints=%d", srv.count())
	}
	// Now within the 5m skew of the 60m expiry → must refresh.
	clock.advance(6 * time.Minute) // now base+56m; refresh threshold base+55m
	t2, _ := p.TokenFor(context.Background(), "wsA")
	if srv.count() != 2 {
		t.Fatalf("did not refresh within skew of expiry; mints=%d, want 2", srv.count())
	}
	if t1 == t2 {
		t.Fatalf("refresh returned the same stale token %q", t1)
	}
}

// PROPERTY 1d — PER-WORKSPACE ISOLATION: distinct workspaces get distinct tokens, each
// minted once and cached independently.
func TestTokenFor_IsolatesPerWorkspace(t *testing.T) {
	clock := newClock()
	srv := newMintServer(t, clock)
	defer srv.Close()
	p := New(srv.URL, "admin-key", Options{TTL: time.Hour, Skew: 5 * time.Minute, Now: clock.now, HTTP: srv.Client()})

	a, _ := p.TokenFor(context.Background(), "wsA")
	b, _ := p.TokenFor(context.Background(), "wsB")
	if a == b {
		t.Fatalf("wsA and wsB got the same token %q — not isolated", a)
	}
	if srv.wsCount("wsA") != 1 || srv.wsCount("wsB") != 1 {
		t.Fatalf("per-ws mint counts wrong: wsA=%d wsB=%d", srv.wsCount("wsA"), srv.wsCount("wsB"))
	}
	// Re-fetching wsA hits its cache; wsB's mint didn't disturb it.
	if _, _ = p.TokenFor(context.Background(), "wsA"); srv.count() != 2 {
		t.Fatalf("total mints=%d, want 2 (one per workspace, both cached after)", srv.count())
	}
}

// PROPERTY 1e — CONCURRENT MINTS COALESCE: many goroutines racing TokenFor for the SAME
// cold workspace must trigger exactly ONE mint, not one per caller. This is what makes the
// provider safe under the bounded worker pool.
func TestTokenFor_ConcurrentSameWorkspaceMintsOnce(t *testing.T) {
	clock := newClock()
	srv := newMintServer(t, clock)
	srv.gate = make(chan struct{})
	defer srv.Close()
	p := New(srv.URL, "admin-key", Options{TTL: time.Hour, Skew: 5 * time.Minute, Now: clock.now, HTTP: srv.Client()})

	const N = 20
	var wg sync.WaitGroup
	toks := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], _ = p.TokenFor(context.Background(), "wsA")
		}(i)
	}
	// Give the racers time to pile up on the single in-flight mint, then release.
	time.Sleep(50 * time.Millisecond)
	close(srv.gate)
	wg.Wait()

	if srv.count() != 1 {
		t.Fatalf("concurrent cold TokenFor minted %d times, want exactly 1 (coalesced)", srv.count())
	}
	for i := 1; i < N; i++ {
		if toks[i] != toks[0] {
			t.Fatalf("racers got different tokens: %q vs %q", toks[0], toks[i])
		}
	}
}
