// Package notification implements Docs email notifications: a recipient
// directory, per-event opt-out preferences, and a best-effort dispatcher that
// turns page/approval/freshness events into emails via the shared
// internal/email delivery layer. Strictly opt-in (EMAIL_ENABLED); inert when
// disabled.
package notification

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Recipient is a resolved email address for a member.
type Recipient struct {
	MemberID string
	Email    string
	Name     string
}

// RecipientStore resolves member IDs to email addresses from the
// notification_recipients directory. Docs stores no user emails of its own, so
// this directory is the single source; members absent from it are simply not
// emailed.
type RecipientStore struct {
	pool pgxDB
}

func NewRecipientStore(pool *pgxpool.Pool) *RecipientStore {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newRecipientStore(db)
}

func newRecipientStore(db pgxDB) *RecipientStore { return &RecipientStore{pool: db} }

// EmailsByIDs returns the resolvable recipients for the given member IDs.
// Members with no directory row are omitted (best-effort).
func (s *RecipientStore) EmailsByIDs(ctx context.Context, ids []string) (map[string]Recipient, error) {
	out := make(map[string]Recipient, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT member_id, email, name FROM notification_recipients WHERE member_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("notification: recipients: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rc Recipient
		if err := rows.Scan(&rc.MemberID, &rc.Email, &rc.Name); err != nil {
			return nil, err
		}
		if rc.Email != "" {
			out[rc.MemberID] = rc
		}
	}
	return out, rows.Err()
}

// Upsert records/updates a member's email in the directory. Provided so an
// identity sync (the documented population seam) has an entry point.
func (s *RecipientStore) Upsert(ctx context.Context, r Recipient) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO notification_recipients (member_id, email, name, updated_at)
         VALUES ($1, $2, $3, NOW())
         ON CONFLICT (member_id)
         DO UPDATE SET email = EXCLUDED.email, name = EXCLUDED.name, updated_at = NOW()`,
		r.MemberID, r.Email, r.Name)
	if err != nil {
		return fmt.Errorf("notification: upsert recipient: %w", err)
	}
	return nil
}
