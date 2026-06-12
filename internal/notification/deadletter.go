package notification

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/email"
)

// DeadLetterStore persists email messages the async queue has permanently
// given up on (all delivery attempts exhausted). It satisfies
// email.DeadLetterSink, so the queue can hand off failures durably instead of
// dropping them. Only metadata is stored — never the rendered body — so the
// table stays a lightweight ops surface, not a copy of notification content.
type DeadLetterStore struct {
	pool pgxDB
}

func NewDeadLetterStore(pool *pgxpool.Pool) *DeadLetterStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newDeadLetterStore(db)
}

func newDeadLetterStore(db pgxDB) *DeadLetterStore { return &DeadLetterStore{pool: db} }

// DeadLetter is one permanently-failed delivery, for the admin list surface.
type DeadLetter struct {
	ID         int64     `json:"id"`
	Recipients []string  `json:"recipients"`
	Subject    string    `json:"subject"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error"`
	CreatedAt  time.Time `json:"created_at"`
}

// Record durably stores a give-up. Called from a queue worker goroutine, off
// the request path.
func (s *DeadLetterStore) Record(ctx context.Context, msg email.Message, attempts int, lastErr string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO notification_dead_letters (recipients, subject, attempts, last_error)
         VALUES ($1, $2, $3, $4)`,
		msg.To, msg.Subject, attempts, lastErr,
	)
	if err != nil {
		return fmt.Errorf("notification: record dead letter: %w", err)
	}
	return nil
}

// List returns the most recent dead-lettered messages, newest first.
func (s *DeadLetterStore) List(ctx context.Context, limit int) ([]DeadLetter, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, recipients, subject, attempts, last_error, created_at
         FROM notification_dead_letters
         ORDER BY created_at DESC
         LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("notification: list dead letters: %w", err)
	}
	defer rows.Close()

	var out []DeadLetter
	for rows.Next() {
		var d DeadLetter
		if err := rows.Scan(&d.ID, &d.Recipients, &d.Subject, &d.Attempts, &d.LastError, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
