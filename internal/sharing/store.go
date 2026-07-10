// Package sharing owns public-link sharing — the surface that turns
// a single page into a tokenised URL anyone can open. Tokens are
// UUIDs (non-sequential) and passwords are bcrypt-hashed; the API
// never serialises the hash.
package sharing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/talyvor/docs/internal/permission"
)

// validShareAccess restricts shared-link access to non-mutating
// roles. Granting "edit" on a public link is a footgun — Docs's
// editor wouldn't authenticate the writer anyway.
var validShareAccess = map[permission.AccessLevel]bool{
	permission.AccessView:    true,
	permission.AccessComment: true,
}

const bcryptCost = 10

// ErrShareLinkNotFound is returned when a revoke targets a link that does not belong to the given page
// (or does not exist). The Revoke route's AccessAdmin gate proves the caller admins {pageID}; scoping the
// delete to that page turns a cross-page revoke-by-id into a clean not-found rather than a cross-tenant
// deletion.
var ErrShareLinkNotFound = errors.New("sharing: share link not found for page")

type ShareLink struct {
	ID           string                 `json:"id"`
	PageID       string                 `json:"page_id"`
	WorkspaceID  string                 `json:"workspace_id"`
	Token        string                 `json:"token"`
	Access       permission.AccessLevel `json:"access"`
	ExpiresAt    *time.Time             `json:"expires_at,omitempty"`
	PasswordHash *string                `json:"-"`
	ViewCount    int                    `json:"view_count"`
	CreatedBy    string                 `json:"created_by"`
	CreatedAt    time.Time              `json:"created_at"`
	// HasPassword is the API-safe replacement for PasswordHash.
	// Set when we strip the hash on the way out.
	HasPassword bool `json:"has_password"`
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

const cols = `id, page_id, workspace_id, token, access, expires_at, password_hash, view_count, created_by, created_at`

func scan(s interface{ Scan(...any) error }) (*ShareLink, error) {
	var l ShareLink
	if err := s.Scan(
		&l.ID, &l.PageID, &l.WorkspaceID, &l.Token, &l.Access,
		&l.ExpiresAt, &l.PasswordHash, &l.ViewCount,
		&l.CreatedBy, &l.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &l, nil
}

// stripHash sets HasPassword and clears the bcrypt hash so the
// returned struct is safe to JSON-encode for the API.
func stripHash(l *ShareLink) *ShareLink {
	if l == nil {
		return nil
	}
	l.HasPassword = l.PasswordHash != nil && *l.PasswordHash != ""
	l.PasswordHash = nil
	return l
}

// newToken returns a 128-bit-entropy hex token. Not strictly a UUID
// in the v4 sense, but indistinguishable to an attacker — and we
// avoid the uuid dependency for a single call site.
func newToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// Create generates a token + persists the share link. If password
// is non-empty, it's bcrypt-hashed before storage. The returned
// ShareLink has its hash elided — callers must show the token URL
// to the user immediately (we don't re-expose it later).
func (s *Store) Create(ctx context.Context, pageID, workspaceID, createdBy string, access permission.AccessLevel, expiresAt *time.Time, password string) (*ShareLink, error) {
	if s.pool == nil {
		return nil, errors.New("sharing: no pool")
	}
	if !validShareAccess[access] {
		return nil, fmt.Errorf("sharing: access %q not allowed for share link", access)
	}
	token, err := newToken()
	if err != nil {
		return nil, fmt.Errorf("sharing: token: %w", err)
	}
	var hashPtr *string
	if password != "" {
		h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
		if err != nil {
			return nil, fmt.Errorf("sharing: hash: %w", err)
		}
		hs := string(h)
		hashPtr = &hs
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO share_links
        (page_id, workspace_id, token, access, expires_at, password_hash, created_by)
        VALUES ($1, $2, $3, $4, $5, $6, $7)
        RETURNING `+cols,
		pageID, workspaceID, token, string(access), expiresAt, hashPtr, createdBy,
	)
	link, err := scan(row)
	if err != nil {
		return nil, fmt.Errorf("sharing: insert: %w", err)
	}
	return stripHash(link), nil
}

// Validate looks up a token, checks expiry + password, and bumps
// the view counter. Returns the link (without its password hash)
// on success. Specific error strings ("expired", "password") let
// the handler map to user-friendly responses without leaking
// internals.
func (s *Store) Validate(ctx context.Context, token, password string) (*ShareLink, error) {
	if s.pool == nil {
		return nil, errors.New("sharing: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM share_links WHERE token = $1`,
		token,
	)
	link, err := scan(row)
	if err != nil {
		return nil, fmt.Errorf("sharing: lookup: %w", err)
	}
	if link.ExpiresAt != nil && time.Now().UTC().After(*link.ExpiresAt) {
		return nil, errors.New("sharing: link expired")
	}
	if link.PasswordHash != nil && *link.PasswordHash != "" {
		if password == "" {
			return nil, errors.New("sharing: password required")
		}
		if bcrypt.CompareHashAndPassword([]byte(*link.PasswordHash), []byte(password)) != nil {
			return nil, errors.New("sharing: password mismatch")
		}
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE share_links SET view_count = view_count + 1 WHERE id = $1`,
		link.ID,
	); err != nil {
		// View-count bump failure shouldn't fail the read — log and
		// continue once a logger is wired. Phase 8 keeps it simple.
		_ = err
	}
	return stripHash(link), nil
}

// ListByPage returns every active share link for the page. The
// password hash is elided.
func (s *Store) ListByPage(ctx context.Context, pageID string) ([]ShareLink, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM share_links WHERE page_id = $1 ORDER BY created_at DESC`,
		pageID,
	)
	if err != nil {
		return nil, fmt.Errorf("sharing: list: %w", err)
	}
	defer rows.Close()
	var out []ShareLink
	for rows.Next() {
		l, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *stripHash(l))
	}
	return out, rows.Err()
}

// Revoke deletes a share link, scoped to pageID: the caller's AccessAdmin is verified against {pageID}
// (route gate), NOT against the link {id}, so without the page_id predicate an admin of any page could
// delete any other page's (any tenant's) share link by id. A link not under pageID → ErrShareLinkNotFound.
func (s *Store) Revoke(ctx context.Context, id, pageID string) error {
	if s.pool == nil {
		return errors.New("sharing: no pool")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM share_links WHERE id = $1 AND page_id = $2`, id, pageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrShareLinkNotFound
	}
	return nil
}
