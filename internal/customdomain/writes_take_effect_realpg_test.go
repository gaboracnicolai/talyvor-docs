package customdomain_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/customdomain"
	"github.com/talyvor/docs/internal/testutil"
)

// TWO WRITES IN THIS PACKAGE COULD RUN AND CHANGE NOTHING, AND NOTHING IN THE REPO WOULD SAY SO.
//
// W3.1 finding (9), measured (scripts/w31-cross-package-write-controls.py). `3c8fbb2` closed the
// half a mock can close — the constructor now verifies its expectations, so a statement
// DISAPPEARING reddens the test that names it. The half it cannot reach is a statement that RUNS
// and matches no row: family P (` AND FALSE`) left the whole repo GREEN for both of these, because
// pgxmock never executes SQL and returns the RowsAffected the fixture wrote, so even the
// `tag.RowsAffected() == 0 ⇒ ErrNotFound` branch these two DO have is answered by the test's own
// fixture rather than by Postgres.
//
// WHY IT MATTERS MORE HERE THAN THE ROW COUNT SUGGESTS: a custom domain is a PUBLISHING control.
// `verified` is what flips a hostname from pending to live, and DomainRouter (middleware.go)
// resolves incoming Host headers against this table. A delete that reports success and removes
// nothing leaves a hostname the operator has revoked still serving that workspace's public space.
//
// The rows are read with SQL against the pool, never through a Store getter, and every test
// asserts its precondition first.

// txtStub answers with whatever the test programmed, so Verify's DNS branch is deterministic.
type txtStub struct{ records map[string][]string }

func (s txtStub) LookupTXT(_ context.Context, host string) ([]string, error) {
	return s.records[host], nil
}

func readDomain(t *testing.T, d *testutil.DB, id string) (found, verified bool, ssl string) {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(),
		`SELECT verified, ssl_status FROM custom_domains WHERE id = $1`, id)
	if err != nil {
		t.Fatalf("read custom domain: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&verified, &ssl); err != nil {
			t.Fatalf("scan custom domain: %v", err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read custom domain: %v", err)
	}
	return found, verified, ssl
}

// ⚠ Verify is the write that publishes a hostname. If the UPDATE runs and matches nothing, Verify
// answers ErrNotFound to a domain that is right there, and the operator's TXT record — which is
// correct — never takes.
func TestVerify_ActuallyPersistsTheVerifiedFlag_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")

	// Created through the store so the token is the one the product generated, not one invented
	// here — the TXT answer below has to be the value the product will compare against.
	seed := customdomain.NewStore(d.Pool)
	cd, err := seed.Create(ctx, ws, "docs.example.com", owner, nil)
	if err != nil {
		t.Fatalf("seed custom domain: %v", err)
	}
	if found, verified, _ := readDomain(t, d, cd.ID); !found || verified {
		t.Fatalf("precondition: found=%v verified=%v, want found and NOT verified", found, verified)
	}

	s := customdomain.NewStoreWithResolver(d.Pool,
		txtStub{records: map[string][]string{"docs.example.com": {cd.VerifyToken}}})
	ok, err := s.Verify(ctx, cd.ID, []string{ws})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatal("Verify returned false for a domain whose TXT record carries the exact token the " +
			"store generated")
	}

	found, verified, ssl := readDomain(t, d, cd.ID)
	if !found {
		t.Fatal("the custom_domains row vanished during Verify")
	}
	if !verified {
		t.Fatal("verified is still FALSE after a Verify that returned true — the operator's DNS " +
			"record is correct, the product said so, and the next request will be told the " +
			"domain is unverified all over again")
	}
	if ssl != "active" {
		t.Errorf("ssl_status = %q after a successful Verify, want %q", ssl, "active")
	}
}

// ⚠ THE DESTRUCTIVE ONE, and the one with a live consumer: DomainRouter reads this table on every
// request that arrives with a foreign Host header.
func TestDelete_ActuallyRemovesTheDomain_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	s := customdomain.NewStore(d.Pool)

	cd, err := s.Create(ctx, ws, "docs.example.com", owner, nil)
	if err != nil {
		t.Fatalf("seed custom domain: %v", err)
	}
	if found, _, _ := readDomain(t, d, cd.ID); !found {
		t.Fatal("precondition: no row for the seeded domain — any post-delete read would be " +
			"meaningless")
	}

	if err := s.Delete(ctx, cd.ID, []string{ws}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if found, _, _ := readDomain(t, d, cd.ID); found {
		t.Fatal("the custom_domains row is STILL THERE after a Delete that returned no error — a " +
			"hostname the operator revoked still resolves to this workspace's public content")
	}
}

// ⚠ THE SAME QUESTION THROUGH THE CONSUMER THAT ACTUALLY MATTERS. "The row is gone" and "the
// router no longer answers for that host" are different claims, and the second is the one a
// revoked domain is about. Kept separate so a failure names which of the two broke.
func TestDelete_TheDeletedDomainNoLongerResolves_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	s := customdomain.NewStore(d.Pool)

	cd, err := s.Create(ctx, ws, "docs.example.com", owner, nil)
	if err != nil {
		t.Fatalf("seed custom domain: %v", err)
	}
	if _, err := s.GetByDomain(ctx, "docs.example.com"); err != nil {
		t.Fatalf("precondition: GetByDomain before the delete: %v — the host was never resolvable, "+
			"so the assertion below could not fail", err)
	}

	if err := s.Delete(ctx, cd.ID, []string{ws}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := s.GetByDomain(ctx, "docs.example.com"); err == nil {
		t.Fatal("GetByDomain STILL resolves a host that was just deleted — this is the lookup " +
			"DomainRouter performs on every request carrying that Host header")
	}
}
