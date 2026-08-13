// Package customdomain backs the "host your docs at
// docs.company.com" feature. A workspace adds an external hostname,
// proves control via a DNS TXT record, and the DomainRouter
// middleware then serves public space content for requests with
// that Host header.
//
// SSL termination is the deploy operator's job (Cloudflare, nginx,
// Caddy) — the ssl_status column is purely informational so the UI
// can show whether the cert handshake works yet.
package customdomain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxDomainsPerWorkspace caps custom domains per tenant. The spec
// pins 5; the cap exists so a runaway script can't flood the
// table.
const MaxDomainsPerWorkspace = 5

// ErrNotFound is returned by the admin by-id operations when the
// target row does not exist within the caller's verified workspace
// set. Handlers map it to 404 so a foreign id is indistinguishable
// from a missing one (no cross-workspace existence oracle).
var ErrNotFound = errors.New("customdomain: not found in workspace")

type CustomDomain struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Domain      string    `json:"domain"`
	SpaceID     *string   `json:"space_id,omitempty"`
	Verified    bool      `json:"verified"`
	VerifyToken string    `json:"verify_token"`
	SSLStatus   string    `json:"ssl_status"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TXTResolver is the narrow DNS surface the store calls. The real
// implementation wraps net.Resolver; tests stub it with an
// in-memory map. We keep the context parameter so callers can
// time-bound a lookup if needed.
type TXTResolver interface {
	LookupTXT(ctx context.Context, host string) ([]string, error)
}

// netResolver adapts net.DefaultResolver into our interface. Lives
// in store.go so callers don't need a separate constructor.
type netResolver struct{}

func (netResolver) LookupTXT(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, host)
}

type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool pgxDB
	txt  TXTResolver
}

func NewStore(pool *pgxpool.Pool) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db, netResolver{})
}

// NewStoreWithResolver lets callers (tests, integration suites)
// inject a custom DNS resolver. Production wires netResolver
// implicitly via NewStore.
func NewStoreWithResolver(pool *pgxpool.Pool, txt TXTResolver) *Store {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newStore(db, txt)
}

func newStore(db pgxDB, txt TXTResolver) *Store {
	if txt == nil {
		txt = netResolver{}
	}
	return &Store{pool: db, txt: txt}
}

const cols = `id, workspace_id, domain, space_id, verified, verify_token, ssl_status, created_by, created_at, updated_at`

func scan(s interface{ Scan(...any) error }) (*CustomDomain, error) {
	var d CustomDomain
	if err := s.Scan(
		&d.ID, &d.WorkspaceID, &d.Domain, &d.SpaceID, &d.Verified,
		&d.VerifyToken, &d.SSLStatus, &d.CreatedBy, &d.CreatedAt, &d.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &d, nil
}

// ─── Validation ─────────────────────────────────────

// domainRE accepts hostnames: a-z0-9 + hyphens (not at the
// boundary) per label, ≥ 2 labels separated by ".". Rejects
// protocols, paths, and double-dot typos. Case-insensitive — we
// normalise to lowercase on insert.
var domainRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]+$`)

func isValidDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" || len(d) > 253 {
		return false
	}
	if strings.Contains(d, "..") {
		return false
	}
	return domainRE.MatchString(d)
}

// newToken returns a random hex string prefixed for grep-ability
// when an operator inspects DNS records by hand.
func newToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "talyvor-verify-" + hex.EncodeToString(buf[:]), nil
}

// ─── CRUD ───────────────────────────────────────────

func (s *Store) Create(ctx context.Context, workspaceID, domain, createdBy string, spaceID *string) (*CustomDomain, error) {
	if s.pool == nil {
		return nil, errors.New("customdomain: no pool")
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if !isValidDomain(domain) {
		return nil, fmt.Errorf("customdomain: invalid domain %q", domain)
	}

	// VALIDATE THE MAPPED SPACE before the row exists.
	//
	// A verified custom domain routes to PublicHandler, which serves EVERY page in the mapped
	// space with no authentication: publicIndex lists whatever ListBySpace returns (skipping
	// only templates) and publicPage serves any slug via GetBySlug, whose query carries no
	// privacy predicate. There is no per-page filter to apply either — model.Page has no
	// published/visibility column at all.
	//
	// space_id arrives in the request BODY. The handler authorizes the {wsID} PATH param, but
	// this value was written straight to the row, so two things were possible:
	//
	//  1. CROSS-TENANT PUBLICATION. A tenant could point their own DNS-verified domain at
	//     another workspace's space id and publish that tenant's pages.
	//  2. SILENT PUBLICATION OF A PRIVATE SPACE. Mapping to a space marked Private
	//     world-published it, with no warning anywhere.
	//
	// Both are refused here. This is the CHEAP half of the fix — "don't allow the mapping" —
	// chosen because the full fix (a per-page `published` column defaulting to false, plus a
	// publish action and a filter in both renderer queries) needs a migration. Refusing costs
	// one query and removes the silent case now.
	if spaceID != nil {
		var spaceWS string
		var private bool
		if err := s.pool.QueryRow(ctx,
			`SELECT workspace_id, private FROM spaces WHERE id = $1`, *spaceID,
		).Scan(&spaceWS, &private); err != nil {
			// Unknown space, or an unreadable one: refuse rather than create a mapping whose
			// target cannot be established. Fail-closed.
			return nil, fmt.Errorf("customdomain: space %q not found", *spaceID)
		}
		if spaceWS != workspaceID {
			// Deliberately does not confirm the space exists elsewhere — no cross-tenant
			// existence oracle, matching the 404-not-403 convention used on the read paths.
			return nil, fmt.Errorf("customdomain: space %q not found", *spaceID)
		}
		if private {
			return nil, fmt.Errorf("customdomain: space %q is private — a custom domain "+
				"publishes every page in the space it maps to, and Docs has no per-page "+
				"publish control yet, so mapping a private space would expose all of it. "+
				"Make the space public first, or map a different space", *spaceID)
		}
	}

	// Workspace-scoped quota — bounded to keep a single tenant from
	// flooding the table.
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM custom_domains WHERE workspace_id = $1`, workspaceID,
	).Scan(&count); err != nil {
		return nil, fmt.Errorf("customdomain: quota check: %w", err)
	}
	if count >= MaxDomainsPerWorkspace {
		return nil, fmt.Errorf("customdomain: max %d domains per workspace", MaxDomainsPerWorkspace)
	}

	token, err := newToken()
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO custom_domains
        (workspace_id, domain, space_id, verify_token, created_by)
        VALUES ($1, $2, $3, $4, $5)
        RETURNING `+cols,
		workspaceID, domain, spaceID, token, createdBy,
	)
	return scan(row)
}

func (s *Store) GetByDomain(ctx context.Context, domain string) (*CustomDomain, error) {
	if s.pool == nil {
		return nil, errors.New("customdomain: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM custom_domains WHERE domain = $1`,
		strings.ToLower(strings.TrimSpace(domain)),
	)
	return scan(row)
}

// SpaceIsPublic answers, AT READ TIME, the question Create answers at write time: may this space
// be served to the world?
//
// ⚠ CREATE'S CHECK CANNOT COVER THIS AND IT IS NOT A SECOND COPY OF IT. Create refuses to MAP a
// private space. `private` is a live column: `PATCH /v1/spaces/{spaceID} {"private":true}` is a
// shipped route on the allow-list (space/store.go's `updatable`), and it is the exact action a
// user takes when they mean "stop showing this to the world". Measured on real Postgres in
// privateflip_realpg_test.go: before this method, a space mapped while public and made private
// afterwards kept serving its page TITLES from the index and its page BODIES from /{slug}, to an
// unauthenticated request, indefinitely. A guard that runs once at write time is a guard against
// the state at that instant, not against the state being served.
//
// FAIL-CLOSED in every direction: no pool, a missing space, or a query error → not public. A
// domain that cannot establish its target's publishability serves nothing.
func (s *Store) SpaceIsPublic(ctx context.Context, spaceID string) (found, public bool) {
	if s.pool == nil || spaceID == "" {
		return false, false
	}
	var private bool
	if err := s.pool.QueryRow(ctx,
		`SELECT private FROM spaces WHERE id = $1`, spaceID,
	).Scan(&private); err != nil {
		return false, false
	}
	return true, !private
}

func (s *Store) GetByWorkspace(ctx context.Context, wsIDs []string) ([]CustomDomain, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+cols+` FROM custom_domains WHERE workspace_id = ANY($1) ORDER BY created_at ASC`,
		wsIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("customdomain: list: %w", err)
	}
	defer rows.Close()
	var out []CustomDomain
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// Verify runs a DNS TXT lookup for the domain and checks for the
// expected verify_token. Already-verified records return true
// without re-querying DNS, so the endpoint can be called
// repeatedly without paying for the lookup every time.
func (s *Store) Verify(ctx context.Context, id string, wsIDs []string) (bool, error) {
	if s.pool == nil {
		return false, errors.New("customdomain: no pool")
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+cols+` FROM custom_domains WHERE id = $1 AND workspace_id = ANY($2)`,
		id, wsIDs,
	)
	cd, err := scan(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("customdomain: not found: %w", err)
	}
	if cd.Verified {
		return true, nil
	}
	records, err := s.txt.LookupTXT(ctx, cd.Domain)
	if err != nil {
		// DNS failures are not domain failures — surface false +
		// nil so the UI can prompt the user to try again later.
		return false, nil
	}
	for _, r := range records {
		if strings.TrimSpace(r) == cd.VerifyToken {
			tag, err := s.pool.Exec(ctx,
				`UPDATE custom_domains
                SET verified = true, ssl_status = 'active', updated_at = NOW()
                WHERE id = $1 AND workspace_id = ANY($2)`,
				id, wsIDs,
			)
			if err != nil {
				return false, fmt.Errorf("customdomain: mark verified: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return false, ErrNotFound
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) Delete(ctx context.Context, id string, wsIDs []string) error {
	if s.pool == nil {
		return errors.New("customdomain: no pool")
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM custom_domains WHERE id = $1 AND workspace_id = ANY($2)`,
		id, wsIDs,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
