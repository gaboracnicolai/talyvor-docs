package ratelimit_test

import (
	"testing"
	"time"

	"github.com/talyvor/docs/internal/ratelimit"
)

// The LLM endpoints call Lens with Docs's single service key and no balance/quota check of
// any kind (see BUILD_STATE §0 Q3), so this limiter is the only per-tenant LLM control in
// the repository. It bounds RATE, not cost.
//
// Token bucket (golang.org/x/time/rate) over fixed-window: a fixed window admits 2N calls
// across a boundary (N at the end of one window, N at the start of the next), which is
// exactly the burst this exists to stop. A bucket also matches the usage shape — a writer
// fires a few AI calls then idles, and should not be punished for the idle time.

func TestAllow_BurstThenDeny(t *testing.T) {
	// 60/min = 1/s refill, burst 3 → 3 immediate, the 4th denied (refill is ~1s away).
	l := ratelimit.New(60, 3)

	for i := 1; i <= 3; i++ {
		if !l.Allow("ws-a") {
			t.Fatalf("call %d/3 within burst was denied, want allowed", i)
		}
	}
	if l.Allow("ws-a") {
		t.Error("call 4 exceeded the burst of 3 but was allowed — the limiter does not bound anything")
	}
}

// PER-TENANT ISOLATION: one workspace exhausting its bucket must not affect another.
// Without this, the limiter becomes a cross-tenant DoS: one noisy tenant starves the rest.
func TestAllow_KeysAreIsolated(t *testing.T) {
	l := ratelimit.New(60, 2)

	for i := 0; i < 2; i++ {
		if !l.Allow("ws-a") {
			t.Fatalf("ws-a call %d denied inside its burst", i)
		}
	}
	if l.Allow("ws-a") {
		t.Fatal("ws-a is not exhausted — fixture wrong, the isolation assert below would be meaningless")
	}
	// ws-b has spent nothing.
	for i := 0; i < 2; i++ {
		if !l.Allow("ws-b") {
			t.Errorf("ws-b call %d denied because ws-a exhausted ITS bucket — buckets are not per-key, "+
				"so one tenant starves every other", i)
		}
	}
}

func TestAllow_RefillsOverTime(t *testing.T) {
	// 600/min = 10/s → a token every 100ms.
	l := ratelimit.New(600, 1)
	if !l.Allow("ws-a") {
		t.Fatal("first call denied")
	}
	if l.Allow("ws-a") {
		t.Fatal("second immediate call allowed — burst is 1")
	}
	time.Sleep(150 * time.Millisecond)
	if !l.Allow("ws-a") {
		t.Error("call after a refill interval was denied — the bucket never refills, so a workspace " +
			"is permanently locked out after one burst")
	}
}

// A limiter that never forgets a key is a memory leak: workspace ids are unbounded over a
// deployment's life. Idle buckets must be evictable.
func TestEviction_DropsIdleBuckets(t *testing.T) {
	l := ratelimit.New(600, 1).WithTTL(50 * time.Millisecond)

	l.Allow("ws-a")
	l.Allow("ws-b")
	if got := l.Buckets(); got != 2 {
		t.Fatalf("Buckets() = %d, want 2", got)
	}
	time.Sleep(80 * time.Millisecond)
	// Touching an unrelated key drives the sweep; ws-a/ws-b are now idle past their TTL.
	l.Allow("ws-c")
	if got := l.Buckets(); got > 1 {
		t.Errorf("Buckets() = %d after the TTL elapsed, want the idle buckets evicted (<=1). "+
			"An unevicted map grows without bound for the life of the process.", got)
	}
}

// An evicted bucket must come back FULL, not carry a stale debt.
func TestEviction_ReadmittedKeyStartsFresh(t *testing.T) {
	l := ratelimit.New(600, 2).WithTTL(50 * time.Millisecond)
	l.Allow("ws-a")
	l.Allow("ws-a")
	if l.Allow("ws-a") {
		t.Fatal("ws-a not exhausted — fixture wrong")
	}
	time.Sleep(80 * time.Millisecond)
	l.Allow("ws-c") // drive the sweep
	if !l.Allow("ws-a") {
		t.Error("a re-admitted key was still denied — eviction must not resurrect a spent bucket")
	}
}

// A zero/negative rate must not silently mean "unlimited" — that is the fail-open shape.
func TestNew_RejectsNonPositiveRateByFailingClosed(t *testing.T) {
	l := ratelimit.New(0, 0)
	if l.Allow("ws-a") {
		t.Error("New(0,0) allowed a call — a misconfigured limiter must fail CLOSED, not become a no-op")
	}
}
