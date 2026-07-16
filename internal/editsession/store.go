// Package editsession is the single-writer policy seam for Option A. It owns exactly ONE
// decision: who may write a page right now.
//
// A page has at most one edit session (page_edit_sessions, page_id PK). A session is LIVE iff
// its last_heartbeat is within TTL of now. The holder saves normally; a non-holder is rejected
// while the holder's session is live ("<holder> is editing"). An expired or absent session is
// claimable (Acquire / Takeover). A LIVE session is never stolen.
//
// COMPOSE, DON'T REPLACE: this does not gate the save path by itself. main.go composes it with
// the manual pagelock (approval + explicit lock) via Composite, so the store guard is
// approvalOK AND manualLockOK AND editSessionOK. The approval gate, the manual pagelock, and
// the append-only version history are all unchanged.
//
// THE OPTION-B SEAM: Option B (real-time multi-writer: presence + CRDT/ProseMirror merge)
// swaps ONLY MayWrite — single holder becomes many concurrent writers — without touching the
// save-commit seam (page.Store.Update), the version model, approval, or the manual lock. The
// linear append-only history stays; B appends its merged snapshots as versions like any save.
//
// TENANCY is the rail: every op is scoped to the SERVER-authorized workspace set. A page in a
// workspace the caller isn't a member of resolves to ErrNotFound (no cross-tenant acquire,
// observe, or takeover; 403≡404, no existence oracle).
package editsession

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTTL — a session is live iff last_heartbeat > now() - DefaultTTL. Sized for a ~10s
// client heartbeat: a couple of missed beats expires the slot so a crashed/closed editor
// doesn't hold a page hostage, while a normal editor keeps it comfortably.
const DefaultTTL = 30 * time.Second

// ErrNotFound is returned when the page is absent OR lives outside the caller's authorized
// workspaces. Same value for both so the API is a 404 with no cross-tenant existence oracle.
var ErrNotFound = errors.New("editsession: page not found in an accessible workspace")

// Session is the wire + policy shape of a page's current writer slot.
type Session struct {
	PageID        string    `json:"page_id"`
	WorkspaceID   string    `json:"workspace_id"`
	Holder        string    `json:"holder"`
	AcquiredAt    time.Time `json:"acquired_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Live          bool      `json:"live"`
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool pgxDB
	ttl  time.Duration
}

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return &Store{pool: db, ttl: DefaultTTL}
}

// WithTTL overrides the liveness window. Test-facing (fast expiry); production uses DefaultTTL.
func (s *Store) WithTTL(ttl time.Duration) *Store {
	s.ttl = ttl
	return s
}

func (s *Store) ttlSecs() float64 { return s.ttl.Seconds() }

// pageWorkspace is the TENANCY GATE: it returns the page's workspace only if the page lives in
// one of the caller's authorized workspaces, else ErrNotFound. Every public op calls it first,
// so nothing observes or mutates a session across a tenant boundary.
func (s *Store) pageWorkspace(ctx context.Context, pageID string, wsIDs []string) (string, error) {
	var ws string
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM pages WHERE id = $1 AND workspace_id = ANY($2)`,
		pageID, wsIDs,
	).Scan(&ws)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("editsession: scope check: %w", err)
	}
	return ws, nil
}

// Get observes the current session (nil if none). Workspace-scoped.
func (s *Store) Get(ctx context.Context, pageID string, wsIDs []string) (*Session, error) {
	if s.pool == nil {
		return nil, errors.New("editsession: no pool")
	}
	if _, err := s.pageWorkspace(ctx, pageID, wsIDs); err != nil {
		return nil, err
	}
	return s.read(ctx, pageID)
}

// read fetches the raw session row (no tenancy gate — callers gate first). nil if none.
func (s *Store) read(ctx context.Context, pageID string) (*Session, error) {
	var sess Session
	var live bool
	err := s.pool.QueryRow(ctx,
		`SELECT page_id, workspace_id, holder, acquired_at, last_heartbeat,
                last_heartbeat > now() - make_interval(secs => $2)
         FROM page_edit_sessions WHERE page_id = $1`,
		pageID, s.ttlSecs(),
	).Scan(&sess.PageID, &sess.WorkspaceID, &sess.Holder, &sess.AcquiredAt, &sess.LastHeartbeat, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("editsession: read: %w", err)
	}
	sess.Live = live
	return &sess, nil
}

// Acquire takes the writer slot for `holder`. Succeeds when the slot is FREE (no row), already
// held by `holder` (refresh), or the existing session is EXPIRED. It NEVER steals a LIVE
// session held by someone else — that returns the current session + a "locked by" style error.
func (s *Store) Acquire(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error) {
	return s.claim(ctx, pageID, wsIDs, holder)
}

// Takeover claims a slot whose session is expired or absent. Semantically identical to Acquire
// on the safety-critical axis — it will NOT steal a live session held by another writer — but
// exposed separately as the explicit "the previous editor is gone, take over" affordance.
func (s *Store) Takeover(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error) {
	return s.claim(ctx, pageID, wsIDs, holder)
}

// claim is the shared acquire/takeover core. The UPSERT's WHERE only lets the write land when
// the slot is claimable (mine, or expired) — a live foreign session leaves the row untouched,
// which we detect and report. This is the ONE place the "who may take the slot" rule lives.
func (s *Store) claim(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error) {
	if s.pool == nil {
		return nil, errors.New("editsession: no pool")
	}
	ws, err := s.pageWorkspace(ctx, pageID, wsIDs)
	if err != nil {
		return nil, err
	}
	var sess Session
	err = s.pool.QueryRow(ctx,
		`INSERT INTO page_edit_sessions (page_id, workspace_id, holder, acquired_at, last_heartbeat)
         VALUES ($1, $2, $3, now(), now())
         ON CONFLICT (page_id) DO UPDATE
            SET holder = EXCLUDED.holder, acquired_at = now(), last_heartbeat = now()
            WHERE page_edit_sessions.holder = EXCLUDED.holder
               OR page_edit_sessions.last_heartbeat <= now() - make_interval(secs => $4)
         RETURNING page_id, workspace_id, holder, acquired_at, last_heartbeat`,
		pageID, ws, holder, s.ttlSecs(),
	).Scan(&sess.PageID, &sess.WorkspaceID, &sess.Holder, &sess.AcquiredAt, &sess.LastHeartbeat)
	if errors.Is(err, pgx.ErrNoRows) {
		// The WHERE blocked the update: a LIVE session held by someone else. Report the holder;
		// the live session is not stolen.
		cur, rerr := s.read(ctx, pageID)
		if rerr != nil {
			return nil, rerr
		}
		who := "another user"
		if cur != nil {
			who = cur.Holder
		}
		return cur, fmt.Errorf("%w: %s", ErrHeldByOther, who)
	}
	if err != nil {
		return nil, fmt.Errorf("editsession: claim: %w", err)
	}
	sess.Live = true
	return &sess, nil
}

// ErrHeldByOther means a LIVE session is held by a different writer — the slot was not taken.
var ErrHeldByOther = errors.New("editsession: page is being edited by another writer")

// Heartbeat extends the caller's live session. It refreshes only if `holder` actually holds the
// slot — you cannot heartbeat your way into someone else's session. Returns ErrHeldByOther if
// the slot isn't the caller's.
func (s *Store) Heartbeat(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error) {
	if s.pool == nil {
		return nil, errors.New("editsession: no pool")
	}
	if _, err := s.pageWorkspace(ctx, pageID, wsIDs); err != nil {
		return nil, err
	}
	var sess Session
	err := s.pool.QueryRow(ctx,
		`UPDATE page_edit_sessions SET last_heartbeat = now()
         WHERE page_id = $1 AND holder = $2
         RETURNING page_id, workspace_id, holder, acquired_at, last_heartbeat`,
		pageID, holder,
	).Scan(&sess.PageID, &sess.WorkspaceID, &sess.Holder, &sess.AcquiredAt, &sess.LastHeartbeat)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w", ErrHeldByOther)
	}
	if err != nil {
		return nil, fmt.Errorf("editsession: heartbeat: %w", err)
	}
	sess.Live = true
	return &sess, nil
}

// Release drops the caller's session. Only the holder can release (a no-op otherwise, so a
// stale client releasing after being taken over doesn't disturb the new holder).
func (s *Store) Release(ctx context.Context, pageID string, wsIDs []string, holder string) error {
	if s.pool == nil {
		return errors.New("editsession: no pool")
	}
	if _, err := s.pageWorkspace(ctx, pageID, wsIDs); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM page_edit_sessions WHERE page_id = $1 AND holder = $2`,
		pageID, holder,
	)
	if err != nil {
		return fmt.Errorf("editsession: release: %w", err)
	}
	return nil
}

// MayWrite is the SINGLE-WRITER POLICY — the one place the ephemeral decision lives, workspace-
// scoped. A write is allowed unless a LIVE session is held by someone other than memberID.
// (No session, an expired session, or the caller's own session all allow the write.)
func (s *Store) MayWrite(ctx context.Context, pageID string, wsIDs []string, memberID string) (bool, string, error) {
	if _, err := s.pageWorkspace(ctx, pageID, wsIDs); err != nil {
		return false, "", err
	}
	sess, err := s.read(ctx, pageID)
	if err != nil {
		return false, "", err
	}
	return decide(sess, memberID)
}

// decide is the pure single-writer rule shared by MayWrite and the CanEdit guard adapter.
func decide(sess *Session, memberID string) (bool, string, error) {
	if sess == nil || !sess.Live || sess.Holder == memberID {
		return true, "", nil
	}
	return false, fmt.Sprintf("%s is editing", sess.Holder), nil
}

// CanEdit is the editGuard/LockGuard adapter consumed by page.Store.Update via Composite. It is
// PAGE-scoped (not workspace-gated) because the save path authorizes the page upstream
// (UpdateInWorkspaces → assertInWorkspaces); this only adds the single-writer decision. An admin
// bypasses, mirroring the manual pagelock's admin semantics.
func (s *Store) CanEdit(ctx context.Context, pageID, memberID string, isAdmin bool) (bool, string, error) {
	if s.pool == nil {
		return false, "", errors.New("editsession: no pool")
	}
	if isAdmin {
		return true, "", nil
	}
	sess, err := s.read(ctx, pageID)
	if err != nil {
		return false, "", err
	}
	return decide(sess, memberID)
}
