package customdomain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockStore(t *testing.T, txt TXTResolver) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return newStore(pool, txt), pool
}

func domainCols() []string {
	return []string{
		"id", "workspace_id", "domain", "space_id", "verified",
		"verify_token", "ssl_status", "created_by", "created_at", "updated_at",
	}
}

func ptrStr(s string) *string { return &s }

// fakeResolver lets each test programme the TXT response. Maps host
// → list of strings; absent host returns ErrNoTXT.
type fakeResolver struct {
	records map[string][]string
}

func (f *fakeResolver) LookupTXT(_ context.Context, host string) ([]string, error) {
	if recs, ok := f.records[host]; ok {
		return recs, nil
	}
	return nil, errors.New("no such host")
}

// ─── Create ─────────────────────────────────────────

func TestCreate_GeneratesVerifyTokenWithPrefix(t *testing.T) {
	store, pool := newMockStore(t, &fakeResolver{})
	now := time.Now().UTC()

	// No existing rows for the workspace (under the 5 limit).
	pool.ExpectQuery(`SELECT COUNT.*FROM custom_domains WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int(0)))

	pool.ExpectQuery(`INSERT INTO custom_domains`).
		WithArgs("ws-1", "docs.company.com", (*string)(nil), pgxmock.AnyArg(), "u-admin").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"d-1", "ws-1", "docs.company.com", (*string)(nil), false,
			"talyvor-verify-abc123", "pending", "u-admin", now, now,
		))

	cd, err := store.Create(context.Background(), "ws-1", "docs.company.com", "u-admin", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(cd.VerifyToken, "talyvor-verify-") {
		t.Fatalf("token missing prefix: %q", cd.VerifyToken)
	}
	if cd.Verified || cd.SSLStatus != "pending" {
		t.Fatalf("unexpected state: %+v", cd)
	}
}

func TestCreate_RejectsInvalidDomains(t *testing.T) {
	store, _ := newMockStore(t, &fakeResolver{})
	cases := []string{
		"",
		"https://docs.company.com", // protocol not allowed
		"docs.company.com/path",    // path not allowed
		"not a domain",
		"docs..company.com",
	}
	for _, d := range cases {
		_, err := store.Create(context.Background(), "ws-1", d, "u", nil)
		if err == nil {
			t.Errorf("expected rejection of %q", d)
		}
	}
}

func TestCreate_RejectsBeyondMaxPerWorkspace(t *testing.T) {
	store, pool := newMockStore(t, &fakeResolver{})
	pool.ExpectQuery(`SELECT COUNT.*FROM custom_domains WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int(MaxDomainsPerWorkspace)))
	_, err := store.Create(context.Background(), "ws-1", "docs.example.com", "u", nil)
	if err == nil {
		t.Fatal("expected quota error past MaxDomainsPerWorkspace")
	}
}

// ─── GetByDomain ────────────────────────────────────

func TestGetByDomain_ReturnsRecord(t *testing.T) {
	store, pool := newMockStore(t, &fakeResolver{})
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM custom_domains WHERE domain`).
		WithArgs("docs.company.com").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"d-1", "ws-1", "docs.company.com", ptrStr("sp-1"), true,
			"talyvor-verify-xyz", "active", "u-admin", now, now,
		))
	cd, err := store.GetByDomain(context.Background(), "docs.company.com")
	if err != nil {
		t.Fatalf("GetByDomain: %v", err)
	}
	if cd.Verified != true || cd.SpaceID == nil || *cd.SpaceID != "sp-1" {
		t.Fatalf("unexpected: %+v", cd)
	}
}

// ─── Verify ─────────────────────────────────────────

func TestVerify_TxtMatch_FlipsVerifiedAndSSL(t *testing.T) {
	now := time.Now().UTC()
	resolver := &fakeResolver{
		records: map[string][]string{
			"docs.company.com": {"talyvor-verify-abc123", "v=spf1 ..."},
		},
	}
	store, pool := newMockStore(t, resolver)

	// Look up current row.
	pool.ExpectQuery(`SELECT.*FROM custom_domains WHERE id`).
		WithArgs("d-1").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"d-1", "ws-1", "docs.company.com", (*string)(nil), false,
			"talyvor-verify-abc123", "pending", "u-admin", now, now,
		))
	// UPDATE flips verified + ssl_status.
	pool.ExpectExec(`UPDATE custom_domains SET verified = true`).
		WithArgs("d-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	verified, err := store.Verify(context.Background(), "d-1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !verified {
		t.Fatal("expected verified=true on match")
	}
}

func TestVerify_NoMatch_ReturnsFalse(t *testing.T) {
	now := time.Now().UTC()
	resolver := &fakeResolver{
		records: map[string][]string{
			"docs.company.com": {"unrelated-record"},
		},
	}
	store, pool := newMockStore(t, resolver)
	pool.ExpectQuery(`SELECT.*FROM custom_domains WHERE id`).
		WithArgs("d-1").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"d-1", "ws-1", "docs.company.com", (*string)(nil), false,
			"talyvor-verify-abc123", "pending", "u-admin", now, now,
		))

	verified, err := store.Verify(context.Background(), "d-1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified {
		t.Fatal("expected verified=false without matching TXT")
	}
}

func TestVerify_AlreadyVerifiedIsIdempotent(t *testing.T) {
	now := time.Now().UTC()
	resolver := &fakeResolver{} // not consulted — already verified
	store, pool := newMockStore(t, resolver)
	pool.ExpectQuery(`SELECT.*FROM custom_domains WHERE id`).
		WithArgs("d-1").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"d-1", "ws-1", "docs.company.com", (*string)(nil), true,
			"talyvor-verify-abc123", "active", "u-admin", now, now,
		))

	verified, err := store.Verify(context.Background(), "d-1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !verified {
		t.Fatal("already-verified record should report verified=true")
	}
}

// ─── Delete ─────────────────────────────────────────

func TestDelete_RemovesByIDWithinWorkspace(t *testing.T) {
	store, pool := newMockStore(t, &fakeResolver{})
	pool.ExpectExec(`DELETE FROM custom_domains WHERE id`).
		WithArgs("d-1", "ws-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.Delete(context.Background(), "d-1", "ws-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// ─── isValidDomain unit ────────────────────────────

func TestIsValidDomain_AcceptsHostnamesRejectsExtras(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"docs.company.com", true},
		{"a.b.c.d", true},
		{"docs", false},
		{"http://docs.example.com", false},
		{"docs.example.com/path", false},
		{"-bad.example.com", false},
		{"docs..example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isValidDomain(c.in); got != c.want {
			t.Errorf("isValidDomain(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
