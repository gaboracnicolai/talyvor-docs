// Package sharing owns public-link sharing — the surface that turns
// a single page into a tokenised URL anyone can open. A token is a
// random nonce plus an HMAC of it under a server key; only the nonce
// is stored, so a token is refused unless this server signed it and
// the share_links table alone does not hold a working link.
// Passwords are bcrypt-hashed; the API never serialises the hash.
package sharing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

// ErrInvalidToken is returned by Validate for a token this server did not sign: a changed nonce, a
// changed or missing signature. It is refused before any lookup, and the handler answers it with
// the same 404 as an unknown link.
var ErrInvalidToken = errors.New("sharing: invalid token")

// signedNoncePrefix marks a nonce minted since tokens were signed. Such a nonce is accepted only with
// its signature, so cutting the signature off a token does not leave a working bare one. A bare
// token without the prefix is a link made before signing, and only the lookup can judge it.
const signedNoncePrefix = "s1_"

// shareKeyLabel keeps the share-token key apart from every other use of the secret it is derived from.
const shareKeyLabel = "talyvor-docs share-token v1"

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

type Store struct {
	pool pgxDB
	key  []byte // HMAC key for share tokens; nil means Create refuses and signed tokens are refused
}

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db)
}

func newStore(db pgxDB) *Store { return &Store{pool: db} }

// WithSigningSecret sets the secret share tokens are signed under. main.go passes
// GATEWAY_AUTH_SECRET, which every deployment already has; the key is an HMAC of a fixed label under
// it, so the secret itself never signs anything. Rotating that secret retires every share link.
func (s *Store) WithSigningSecret(secret string) *Store {
	s.key = nil
	if secret != "" {
		m := hmac.New(sha256.New, []byte(secret))
		m.Write([]byte(shareKeyLabel))
		s.key = m.Sum(nil)
	}
	return s
}

func (s *Store) mac(nonce string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(nonce))
	return m.Sum(nil)
}

// sign turns a stored nonce into the token a share URL carries.
func (s *Store) sign(nonce string) string {
	return nonce + "." + base64.RawURLEncoding.EncodeToString(s.mac(nonce))
}

// nonceOf returns the stored nonce a presented token names, or false for a token this server did not
// sign. See signedNoncePrefix for the bare case.
func (s *Store) nonceOf(token string) (string, bool) {
	nonce, sig, signed := strings.Cut(token, ".")
	if !signed {
		return token, token != "" && !strings.HasPrefix(token, signedNoncePrefix)
	}
	if s.key == nil || nonce == "" {
		return "", false
	}
	// Strict: the last character of a 32-byte MAC carries two spare bits, and a lenient decoder
	// ignores them, so four spellings of one signature would all verify.
	got, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	if err != nil {
		return "", false
	}
	return nonce, hmac.Equal(got, s.mac(nonce))
}

// present puts the signed token on a link read from the database, which holds only the nonce.
func (s *Store) present(l *ShareLink) *ShareLink {
	if l != nil && s.key != nil {
		l.Token = s.sign(l.Token)
	}
	return l
}

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

// newNonce returns a 128-bit-entropy hex nonce with the signed-era prefix. It is what share_links.token
// stores; the URL carries it signed (sign).
func newNonce() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return signedNoncePrefix + hex.EncodeToString(buf[:]), nil
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
	if s.key == nil {
		return nil, errors.New("sharing: no signing key")
	}
	token, err := newNonce()
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
	return s.present(stripHash(link)), nil
}

// Validate checks the token's signature, looks up its nonce, checks
// expiry + password, and bumps the view counter. Returns the link (without its password hash)
// on success. Specific error strings ("expired", "password") let
// the handler map to user-friendly responses without leaking
// internals.
func (s *Store) Validate(ctx context.Context, token, password string) (*ShareLink, error) {
	if s.pool == nil {
		return nil, errors.New("sharing: no pool")
	}
	nonce, ok := s.nonceOf(token)
	if !ok {
		return nil, ErrInvalidToken
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM share_links WHERE token = $1`,
		nonce,
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
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- bearer-secret pattern, not an IDOR surface: link.ID is not client-supplied, it is read off the row already found by `WHERE token = $1` (128-bit CSPRNG token) with the expiry + bcrypt checks passed above. There is no attacker-controlled id to scope.
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
		out = append(out, *s.present(stripHash(l)))
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
	// nosemgrep: docs-by-id-write-requires-workspace-scope -- FALSE POSITIVE: this IS scoped, by `AND page_id = $2` (the ce8bfe3 cross-tenant fix). The rule's pattern-not exclusion only recognises `workspace_id = ANY`, so it cannot see a page-scoped write. share_links is scoped THROUGH its page, and the caller's page is verified upstream by pageEnf.Require(AccessAdmin) (handler.go Mount); a foreign link id matches 0 rows → ErrShareLinkNotFound → 404.
	tag, err := s.pool.Exec(ctx, `DELETE FROM share_links WHERE id = $1 AND page_id = $2`, id, pageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrShareLinkNotFound
	}
	return nil
}
