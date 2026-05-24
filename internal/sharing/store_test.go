package sharing

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return newStore(pool), pool
}

func TestCreate_GeneratesUUIDTokenAndStoresRow(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectQuery(`INSERT INTO share_links`).
		WithArgs(
			"pg-1", "ws-1",
			pgxmock.AnyArg(), // token (uuid)
			"view",
			(*time.Time)(nil), // no expiry
			(*string)(nil),    // no password
			"u-admin",
		).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "page_id", "workspace_id", "token", "access",
			"expires_at", "password_hash", "view_count",
			"created_by", "created_at",
		}).AddRow(
			"sl-1", "pg-1", "ws-1", "tok-abc", "view",
			(*time.Time)(nil), (*string)(nil), 0,
			"u-admin", time.Now().UTC(),
		))

	link, err := store.Create(context.Background(), "pg-1", "ws-1", "u-admin", "view", nil, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.Token == "" {
		t.Fatal("expected non-empty token")
	}
	if link.PasswordHash != nil {
		t.Fatalf("password_hash leaked: %+v", *link.PasswordHash)
	}
}

func TestCreate_RejectsInvalidAccess(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.Create(context.Background(), "pg-1", "ws-1", "u-admin", "admin", nil, "")
	if err == nil {
		t.Fatal("share link should reject admin access")
	}
}

func TestCreate_BcryptHashesPassword(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectQuery(`INSERT INTO share_links`).
		WithArgs(
			"pg-1", "ws-1",
			pgxmock.AnyArg(),
			"view",
			(*time.Time)(nil),
			pgxmock.AnyArg(), // hashed password
			"u-admin",
		).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "page_id", "workspace_id", "token", "access",
			"expires_at", "password_hash", "view_count",
			"created_by", "created_at",
		}).AddRow(
			"sl-1", "pg-1", "ws-1", "tok-abc", "view",
			(*time.Time)(nil), ptrStr("$2a$10$xyz"), 0,
			"u-admin", time.Now().UTC(),
		))

	link, err := store.Create(context.Background(), "pg-1", "ws-1", "u-admin", "view", nil, "hunter2")
	if err != nil {
		t.Fatalf("Create with password: %v", err)
	}
	if link.PasswordHash != nil {
		// The store should strip password_hash before returning so it
		// never reaches the API caller. The DB row carries it but the
		// returned struct has it elided.
		t.Fatalf("password_hash exposed to caller: %v", *link.PasswordHash)
	}
}

func TestValidate_AcceptsCorrectPassword(t *testing.T) {
	store, pool := newMockStore(t)
	// Hash for "hunter2", cost 10.
	hash := "$2a$10$oZaKnSRc0whLbqE.wQlM7e82YG89I1y2RcRSkCRwe4pIt9iIRFMAG"

	pool.ExpectQuery(`SELECT.*FROM share_links WHERE token`).
		WithArgs("tok-abc").
		WillReturnRows(rowsForLink(hash, nil))

	pool.ExpectExec(`UPDATE share_links SET view_count`).
		WithArgs("sl-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	link, err := store.Validate(context.Background(), "tok-abc", "hunter2")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if link == nil || link.PasswordHash != nil {
		t.Fatalf("password_hash should be elided in response: %+v", link)
	}
}

func TestValidate_RejectsWrongPassword(t *testing.T) {
	store, pool := newMockStore(t)
	hash := "$2a$10$oZaKnSRc0whLbqE.wQlM7e82YG89I1y2RcRSkCRwe4pIt9iIRFMAG"

	pool.ExpectQuery(`SELECT.*FROM share_links WHERE token`).
		WithArgs("tok-abc").
		WillReturnRows(rowsForLink(hash, nil))

	_, err := store.Validate(context.Background(), "tok-abc", "wrong")
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected password mismatch error, got %v", err)
	}
}

func TestValidate_RejectsExpired(t *testing.T) {
	store, pool := newMockStore(t)
	past := time.Now().UTC().Add(-time.Hour)

	pool.ExpectQuery(`SELECT.*FROM share_links WHERE token`).
		WithArgs("tok-abc").
		WillReturnRows(rowsForLink("", &past))

	_, err := store.Validate(context.Background(), "tok-abc", "")
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func TestValidate_UnknownToken_ReturnsError(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectQuery(`SELECT.*FROM share_links WHERE token`).
		WithArgs("missing").
		WillReturnError(errNoRows())

	_, err := store.Validate(context.Background(), "missing", "")
	if err == nil {
		t.Fatal("expected error for unknown token")
	}
}

func TestRevoke_DeletesRow(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`DELETE FROM share_links`).
		WithArgs("sl-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := store.Revoke(context.Background(), "sl-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
}

// ─── helpers ───

func ptrStr(s string) *string { return &s }

func rowsForLink(hash string, expires *time.Time) *pgxmock.Rows {
	var passField *string
	if hash != "" {
		passField = &hash
	}
	return pgxmock.NewRows([]string{
		"id", "page_id", "workspace_id", "token", "access",
		"expires_at", "password_hash", "view_count",
		"created_by", "created_at",
	}).AddRow(
		"sl-1", "pg-1", "ws-1", "tok-abc", "view",
		expires, passField, 0,
		"u-admin", time.Now().UTC(),
	)
}

// errNoRows mimics the pgx.ErrNoRows sentinel used by QueryRow.Scan
// when no rows match. The store should wrap this into a friendlier
// error for callers.
func errNoRows() error {
	return pgxNoRows{}
}

type pgxNoRows struct{}

func (pgxNoRows) Error() string { return "no rows in result set" }
