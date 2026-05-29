package email

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordSender records delivered messages and lets a test control per-attempt
// outcomes via fn (attempt number → error; nil = success).
type recordSender struct {
	mu    sync.Mutex
	got   []Message
	calls int32
	fn    func(attempt int) error
}

func (r *recordSender) Send(_ context.Context, m Message) error {
	n := atomic.AddInt32(&r.calls, 1)
	if r.fn != nil {
		if err := r.fn(int(n)); err != nil {
			return err
		}
	}
	r.mu.Lock()
	r.got = append(r.got, m)
	r.mu.Unlock()
	return nil
}

func (r *recordSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func ctx2s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestQueue_DeliversAllEnqueued(t *testing.T) {
	rs := &recordSender{}
	q := NewQueue(rs, QueueOptions{Workers: 4, Capacity: 100, Attempts: 3, Backoff: time.Millisecond}, nil)
	q.Start(context.Background())
	for i := 0; i < 20; i++ {
		if !q.Enqueue(Message{To: []string{"a@b.c"}, Subject: "s"}) {
			t.Fatalf("enqueue %d should succeed under capacity", i)
		}
	}
	q.Shutdown(ctx2s(t))
	if rs.count() != 20 {
		t.Fatalf("delivered %d, want 20", rs.count())
	}
}

func TestQueue_FullDropsWithoutBlocking(t *testing.T) {
	rs := &recordSender{}
	// Not started: nothing drains, so the buffer fills and further enqueues drop.
	q := NewQueue(rs, QueueOptions{Workers: 1, Capacity: 2, Attempts: 1, Backoff: time.Millisecond}, nil)

	if !q.Enqueue(Message{Subject: "1"}) {
		t.Fatal("1st enqueue should fit")
	}
	if !q.Enqueue(Message{Subject: "2"}) {
		t.Fatal("2nd enqueue should fit")
	}

	done := make(chan bool, 1)
	go func() { done <- q.Enqueue(Message{Subject: "3"}) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("3rd enqueue should be dropped (return false) when the queue is full")
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked when full — it must be non-blocking")
	}
}

func TestQueue_RetriesThenSucceeds(t *testing.T) {
	rs := &recordSender{fn: func(attempt int) error {
		if attempt < 3 {
			return errors.New("transient")
		}
		return nil
	}}
	q := NewQueue(rs, QueueOptions{Workers: 1, Capacity: 10, Attempts: 3, Backoff: time.Millisecond}, nil)
	q.Start(context.Background())
	q.Enqueue(Message{Subject: "x"})
	q.Shutdown(ctx2s(t))

	if rs.count() != 1 {
		t.Fatalf("want 1 delivered after retries, got %d", rs.count())
	}
	if got := atomic.LoadInt32(&rs.calls); got != 3 {
		t.Fatalf("want 3 attempts, got %d", got)
	}
}

func TestQueue_GivesUpAfterMaxAttempts(t *testing.T) {
	rs := &recordSender{fn: func(int) error { return errors.New("always down") }}
	q := NewQueue(rs, QueueOptions{Workers: 1, Capacity: 10, Attempts: 3, Backoff: time.Millisecond}, nil)
	q.Start(context.Background())
	q.Enqueue(Message{Subject: "x"})
	q.Shutdown(ctx2s(t))

	if rs.count() != 0 {
		t.Fatalf("want 0 delivered (all attempts failed), got %d", rs.count())
	}
	if got := atomic.LoadInt32(&rs.calls); got != 3 {
		t.Fatalf("want exactly 3 attempts then give up, got %d", got)
	}
}

func TestQueue_EnqueueAfterShutdownIsRejected(t *testing.T) {
	q := NewQueue(&recordSender{}, QueueOptions{Workers: 1, Capacity: 4}, nil)
	q.Start(context.Background())
	q.Shutdown(ctx2s(t))
	if q.Enqueue(Message{Subject: "late"}) {
		t.Fatal("enqueue after shutdown must return false, not panic on a closed channel")
	}
}
