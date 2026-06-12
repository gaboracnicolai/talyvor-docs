package email

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// QueueOptions configures the async delivery queue. Zero values fall back to
// the documented defaults.
type QueueOptions struct {
	Workers  int           // worker goroutines (default 4)
	Capacity int           // buffered channel size (default 256)
	Attempts int           // delivery attempts per message (default 3)
	Backoff  time.Duration // base backoff between attempts (default 500ms)
}

func (o QueueOptions) withDefaults() QueueOptions {
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Capacity <= 0 {
		o.Capacity = 256
	}
	if o.Attempts <= 0 {
		o.Attempts = 3
	}
	if o.Backoff <= 0 {
		o.Backoff = 500 * time.Millisecond
	}
	return o
}

// DeadLetterSink records a message the queue has permanently given up on (all
// delivery attempts exhausted). Implementations are typically durable (a DB
// table) so failed notifications are visible to an admin and never silently
// vanish. Record runs in a worker goroutine off the request path.
type DeadLetterSink interface {
	Record(ctx context.Context, msg Message, attempts int, lastErr string) error
}

// Queue delivers messages asynchronously through a bounded worker pool. It
// never blocks the caller: Enqueue is non-blocking and drops (with a log) when
// the buffer is full, because notifications are best-effort and must never
// hold up a core request. SMTP failures are retried with backoff inside the
// workers, off the request path entirely. When all attempts are exhausted the
// message is handed to the dead-letter sink (if configured) rather than lost.
type Queue struct {
	sender     Sender
	opts       QueueOptions
	logger     *slog.Logger
	deadLetter DeadLetterSink

	ch chan Message
	wg sync.WaitGroup

	mu      sync.RWMutex
	closing bool
}

// WithDeadLetter attaches a dead-letter sink. Optional: with no sink, an
// exhausted message is logged and dropped exactly as before (best-effort).
// Returns the queue for chaining. Call before Start.
func (q *Queue) WithDeadLetter(sink DeadLetterSink) *Queue {
	q.deadLetter = sink
	return q
}

func NewQueue(sender Sender, opts QueueOptions, logger *slog.Logger) *Queue {
	if logger == nil {
		logger = slog.Default()
	}
	opts = opts.withDefaults()
	return &Queue{
		sender: sender,
		opts:   opts,
		logger: logger,
		ch:     make(chan Message, opts.Capacity),
	}
}

// Start spawns the worker pool. ctx cancellation only short-circuits inter-
// attempt backoff; draining is driven by Shutdown closing the channel.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.opts.Workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for msg := range q.ch {
				q.deliver(ctx, msg)
			}
		}()
	}
}

// Enqueue submits a message for async delivery. Returns false if the message
// was dropped (queue full or shutting down). Never blocks.
func (q *Queue) Enqueue(msg Message) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closing {
		return false
	}
	select {
	case q.ch <- msg:
		return true
	default:
		q.logger.Warn("email: queue full, dropping message (best-effort)",
			slog.String("subject", msg.Subject), slog.Any("to", msg.To))
		return false
	}
}

func (q *Queue) deliver(ctx context.Context, msg Message) {
	var lastErr error
	for attempt := 1; attempt <= q.opts.Attempts; attempt++ {
		err := q.sender.Send(ctx, msg)
		if err == nil {
			return
		}
		lastErr = err
		if attempt == q.opts.Attempts {
			break
		}
		timer := time.NewTimer(q.opts.Backoff * time.Duration(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}

	// All attempts exhausted. Log, then dead-letter so the failure is durable
	// and visible rather than silently dropped.
	q.logger.Warn("email: delivery failed, giving up",
		slog.Int("attempts", q.opts.Attempts),
		slog.String("subject", msg.Subject),
		slog.String("err", lastErr.Error()))
	if q.deadLetter != nil {
		if derr := q.deadLetter.Record(ctx, msg, q.opts.Attempts, lastErr.Error()); derr != nil {
			q.logger.Error("email: dead-letter record failed (notification truly lost)",
				slog.String("subject", msg.Subject), slog.String("err", derr.Error()))
		}
	}
}

// Shutdown stops accepting new messages, then waits for the workers to drain
// whatever is already buffered (or until ctx expires). The closing flag is set
// under the write lock so no Enqueue can send on the channel after it closes.
func (q *Queue) Shutdown(ctx context.Context) {
	q.mu.Lock()
	if q.closing {
		q.mu.Unlock()
		return
	}
	q.closing = true
	q.mu.Unlock()

	close(q.ch)

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		q.logger.Warn("email: queue drain timed out on shutdown")
	}
}
