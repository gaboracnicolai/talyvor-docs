package notification

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PreferenceStore reads/writes per-member email notification preferences.
// Opt-OUT model: no row means the member receives the email; only an explicit
// email_enabled=false suppresses an event type.
type PreferenceStore struct {
	pool pgxDB
}

func NewPreferenceStore(pool *pgxpool.Pool) *PreferenceStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newPreferenceStore(db)
}

func newPreferenceStore(db pgxDB) *PreferenceStore { return &PreferenceStore{pool: db} }

// IsEnabled reports whether the member wants email for eventType (default true).
func (p *PreferenceStore) IsEnabled(ctx context.Context, memberID, eventType string) (bool, error) {
	var enabled bool
	err := p.pool.QueryRow(ctx,
		`SELECT email_enabled FROM notification_preferences WHERE member_id = $1 AND event_type = $2`,
		memberID, eventType,
	).Scan(&enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return false, fmt.Errorf("notification: preference lookup: %w", err)
	}
	return enabled, nil
}

// EnabledMembers filters memberIDs to those who should receive email for
// eventType — everyone except members with an explicit email_enabled=false row.
func (p *PreferenceStore) EnabledMembers(ctx context.Context, eventType string, memberIDs []string) ([]string, error) {
	if len(memberIDs) == 0 {
		return nil, nil
	}
	rows, err := p.pool.Query(ctx,
		`SELECT member_id FROM notification_preferences
         WHERE event_type = $1 AND member_id = ANY($2) AND email_enabled = false`,
		eventType, memberIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("notification: enabled members: %w", err)
	}
	defer rows.Close()
	optedOut := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		optedOut[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(memberIDs))
	for _, id := range memberIDs {
		if !optedOut[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// Set upserts a member's preference for an event type.
func (p *PreferenceStore) Set(ctx context.Context, memberID, eventType string, enabled bool) error {
	_, err := p.pool.Exec(ctx,
		`INSERT INTO notification_preferences (member_id, event_type, email_enabled, updated_at)
         VALUES ($1, $2, $3, NOW())
         ON CONFLICT (member_id, event_type)
         DO UPDATE SET email_enabled = EXCLUDED.email_enabled, updated_at = NOW()`,
		memberID, eventType, enabled,
	)
	if err != nil {
		return fmt.Errorf("notification: set preference: %w", err)
	}
	return nil
}
