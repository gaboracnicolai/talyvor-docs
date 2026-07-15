// Package pagelock is the single writer of pages.locked,
// pages.locked_by, and pages.locked_at. The lock is soft (DB-backed)
// so it survives server restarts and is visible to every connected
// client. CanEdit composes the lock with the approval lock from
// internal/approval — both block edits, but for different reasons.
package pagelock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LockState is the wire shape returned to the API + the frontend.
// LockedByName is populated by the handler when it has a member
// directory available — the store leaves it empty.
type LockState struct {
	Locked       bool       `json:"locked"`
	LockedBy     *string    `json:"locked_by,omitempty"`
	LockedByName *string    `json:"locked_by_name,omitempty"`
	LockedAt     *time.Time `json:"locked_at,omitempty"`
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct{ pool pgxDB }

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(db pgxDB) *Store { return &Store{pool: db} }

// pageLockRow is the projection used by every read path — every
// caller wants the same four columns.
type pageLockRow struct {
	locked    bool
	lockedBy  *string
	lockedAt  *time.Time
	docStatus string
}

func (s *Store) read(ctx context.Context, pageID string) (*pageLockRow, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT locked, locked_by, locked_at, COALESCE(doc_status, 'draft')
        FROM pages WHERE id = $1`,
		pageID,
	)
	var r pageLockRow
	if err := row.Scan(&r.locked, &r.lockedBy, &r.lockedAt, &r.docStatus); err != nil {
		return nil, fmt.Errorf("pagelock: read: %w", err)
	}
	return &r, nil
}

// Lock claims the page for `lockedBy`. Idempotent when the same
// member already holds the lock; conflicts when somebody else does.
func (s *Store) Lock(ctx context.Context, pageID, lockedBy string) (*LockState, error) {
	if s.pool == nil {
		return nil, errors.New("pagelock: no pool")
	}
	r, err := s.read(ctx, pageID)
	if err != nil {
		return nil, err
	}
	if r.locked {
		if r.lockedBy != nil && *r.lockedBy == lockedBy {
			return &LockState{Locked: true, LockedBy: r.lockedBy, LockedAt: r.lockedAt}, nil
		}
		holder := "another user"
		if r.lockedBy != nil {
			holder = *r.lockedBy
		}
		return nil, fmt.Errorf("pagelock: page is already locked by %s", holder)
	}
	row := s.pool.QueryRow(ctx,
		`UPDATE pages
        SET locked = true, locked_by = $1, locked_at = NOW()
        WHERE id = $2
        RETURNING locked, locked_by, locked_at, COALESCE(doc_status, 'draft')`,
		lockedBy, pageID,
	)
	var out pageLockRow
	if err := row.Scan(&out.locked, &out.lockedBy, &out.lockedAt, &out.docStatus); err != nil {
		return nil, fmt.Errorf("pagelock: write lock: %w", err)
	}
	return &LockState{Locked: out.locked, LockedBy: out.lockedBy, LockedAt: out.lockedAt}, nil
}

// Unlock releases the lock. The locker can always unlock; an admin
// can always unlock. Anyone else gets an error.
func (s *Store) Unlock(ctx context.Context, pageID, memberID string, isAdmin bool) error {
	if s.pool == nil {
		return errors.New("pagelock: no pool")
	}
	r, err := s.read(ctx, pageID)
	if err != nil {
		return err
	}
	if !r.locked {
		return nil
	}
	if !isAdmin && (r.lockedBy == nil || *r.lockedBy != memberID) {
		return errors.New("pagelock: only the locker or an admin can unlock")
	}
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- EXTERNALLY GATED (this package has no workspace concept; read() above is unscoped too): the sole caller is handler.go Unlock, whose route is wrapped in pageEnf.Require(permission.AccessEdit) in this package's Mount → GetByIDInWorkspaces in cmd/docs/main.go 404s a foreign pageID before the handler runs. The isAdmin branch skips only the locker check above, never the route middleware, so both branches are equally cross-tenant-safe. NOTE: the gate is main.go's WithAccess wiring, NOT this file (Enforcer.Require is pass-through on a nil receiver). isAdmin is supplied by handler.go from permission.IsAdminFromContext (the gateway-verified identity resolved against the permission model) — it was previously read from the request body, which let any Edit-tier member steal a lock.
	_, err = s.pool.Exec(ctx,
		`UPDATE pages SET locked = false, locked_by = NULL, locked_at = NULL
        WHERE id = $1`,
		pageID,
	)
	return err
}

// GetLockState surfaces the current state of the lock. Returns an
// "unlocked" LockState rather than nil when the page exists but has
// no lock — keeps the API shape consistent.
func (s *Store) GetLockState(ctx context.Context, pageID string) (*LockState, error) {
	if s.pool == nil {
		return nil, errors.New("pagelock: no pool")
	}
	r, err := s.read(ctx, pageID)
	if err != nil {
		return nil, err
	}
	return &LockState{Locked: r.locked, LockedBy: r.lockedBy, LockedAt: r.lockedAt}, nil
}

// CanEdit composes the lock + approval rules. Returns
// (canEdit, reason, error). The reason is the user-facing string the
// handler / banner shows when canEdit is false.
//
// Order matters: approval lock wins over user lock for the message
// because approval is a stronger constraint (regulatory) than an
// editor-held lock (collaboration).
func (s *Store) CanEdit(ctx context.Context, pageID, memberID string, isAdmin bool) (bool, string, error) {
	if s.pool == nil {
		return false, "", errors.New("pagelock: no pool")
	}
	r, err := s.read(ctx, pageID)
	if err != nil {
		return false, "", err
	}
	if r.docStatus == "approved" {
		// Approval lock cannot be unlocked by an admin via this
		// path — the user has to re-open the doc through the
		// approval workflow.
		return false, "Page is approved. Edit to request changes.", nil
	}
	if !r.locked {
		return true, "", nil
	}
	if r.lockedBy != nil && *r.lockedBy == memberID {
		return true, "", nil
	}
	if isAdmin {
		return true, "", nil
	}
	holder := "another user"
	if r.lockedBy != nil {
		holder = *r.lockedBy
	}
	return false, fmt.Sprintf("Locked by %s", holder), nil
}
