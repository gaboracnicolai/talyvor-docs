package notification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/email"
)

// dbDirectory is the production directory backed by the pages +
// notification_recipients tables. Read-only; runs off the request/job path.
type dbDirectory struct {
	pool pgxDB
}

func newDBDirectory(pool *pgxpool.Pool) *dbDirectory {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &dbDirectory{pool: db}
}

func (d *dbDirectory) PageByID(ctx context.Context, id string) (*PageRef, error) {
	var r PageRef
	err := d.pool.QueryRow(ctx,
		`SELECT id, space_id, workspace_id, title, created_by FROM pages WHERE id = $1`, id,
	).Scan(&r.ID, &r.SpaceID, &r.WorkspaceID, &r.Title, &r.CreatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, fmt.Errorf("notification: page by id: %w", err)
	}
	return &r, nil
}

// ResolveMentions maps @handles to member IDs by matching the recipient
// directory's email local-part or name (spaces removed), case-insensitive.
// Docs has no per-workspace member list, so the directory is the resolver;
// workspaceID is accepted for interface symmetry but not used for filtering.
func (d *dbDirectory) ResolveMentions(ctx context.Context, _ string, handles []string) ([]string, error) {
	if len(handles) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx,
		`SELECT member_id FROM notification_recipients
         WHERE lower(split_part(email, '@', 1)) = ANY($1)
            OR lower(replace(name, ' ', '')) = ANY($1)`, handles)
	if err != nil {
		return nil, fmt.Errorf("notification: resolve mentions: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// NewDispatcher builds the production dispatcher wired to the database.
func NewDispatcher(pool *pgxpool.Pool, recipients *RecipientStore, prefs *PreferenceStore, queue enqueuer, renderer *email.Renderer, baseURL, appName string, logger *slog.Logger) *Dispatcher {
	return newDispatcher(newDBDirectory(pool), recipients, prefs, queue, renderer, baseURL, appName, logger)
}
