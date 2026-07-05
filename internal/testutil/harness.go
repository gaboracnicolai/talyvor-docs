// Package testutil is Docs's real-Postgres integration-test harness. TEST-INFRA
// ONLY — it holds no production behaviour. It mirrors talyvor-track's
// internal/testutil: New(t) stands up an isolated per-test database, applies every
// migration in order, hands back a live pool, and drops the database on cleanup
// (pass or fail). Docs has no Go migration runner (production applies the
// migrations/ dir via Postgres's initdb.d), so this harness applies the embedded
// migrations directly — the minimal mirror of Track's embedded-FS approach.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/migrations"
)

// DB is one isolated test database + a pool onto it.
type DB struct {
	Pool  *pgxpool.Pool
	admin string // admin DSN (points at a maintenance DB) used to create/drop
	name  string // the per-test database name
}

// New provisions an isolated database, applies the full schema, and returns a
// live pool. Skips (not fails) when DOCS_TEST_DATABASE_URL is unset so unit-only
// runs stay green. The cleanup drops the database whether the test passes or fails.
//
// DOCS_TEST_DATABASE_URL must point at a pgvector/pgvector:pg16-class Postgres:
// the migrations require the `vector` extension (0001_core.sql, 0004_search.sql)
// plus uuid-ossp + pg_trgm. A missing `vector` extension fails LOUD (below) rather
// than as a cryptic mid-migration error.
func New(t *testing.T) *DB {
	t.Helper()
	admin := os.Getenv("DOCS_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("DOCS_TEST_DATABASE_URL not set — skipping real-Postgres integration test")
	}
	ctx := context.Background()
	name := "docs_test_" + randToken(t) // unique per New() → parallel-safe

	// 1. Create the isolated database from the admin connection (idempotent pre-drop).
	admConn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("testutil: admin connect (is DOCS_TEST_DATABASE_URL reachable?): %v", err)
	}
	if _, err := admConn.Exec(ctx, dropDatabaseStmt(name)); err != nil {
		t.Fatalf("testutil: pre-drop %s: %v", name, err)
	}
	if _, err := admConn.Exec(ctx, createDatabaseStmt(name)); err != nil {
		t.Fatalf("testutil: create database %s: %v", name, err)
	}
	_ = admConn.Close(ctx)

	// 2. Pool onto the new database.
	poolCfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatalf("testutil: parse pool config: %v", err)
	}
	poolCfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("testutil: open pool: %v", err)
	}
	d := &DB{Pool: pool, admin: admin, name: name}
	t.Cleanup(d.teardown) // registered NOW so a later failure still drops the DB

	// 3. Fail LOUD if the pgvector extension is unavailable — the migrations need it,
	//    and a clear message beats a cryptic "type vector does not exist" mid-apply.
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("testutil: the `vector` extension is required by the docs migrations but is not "+
			"available on this server. Point DOCS_TEST_DATABASE_URL at a pgvector/pgvector:pg16-class "+
			"Postgres. underlying error: %v", err)
	}

	// 4. Apply every migration in NNNN order.
	applyMigrations(t, ctx, pool)
	return d
}

// applyMigrations runs each embedded *.sql file in lexicographic (== numeric NNNN)
// order. Each file is executed as one simple-protocol statement batch (no args),
// which is how Postgres's initdb.d runs them in production.
func applyMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("testutil: read embedded migrations: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("testutil: read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("testutil: apply migration %s: %v", f, err)
		}
	}
}

// teardown closes the pool and drops the database. Registered via t.Cleanup so it
// runs on pass or fail — no leaked databases.
func (d *DB) teardown() {
	d.Pool.Close()
	ctx := context.Background()
	admConn, err := pgx.Connect(ctx, d.admin)
	if err != nil {
		return
	}
	defer admConn.Close(ctx)
	_, _ = admConn.Exec(ctx, dropDatabaseStmt(d.name))
}

// ---- seed helpers (only what the SEC-4 cross-tenant test needs) ----

// Workspace returns a fresh workspace id. Docs owns no workspaces table —
// workspace_id is an external Track id carried as a bare TEXT on every row — so a
// test only needs a stable unique value to scope rows by.
func (d *DB) Workspace(t *testing.T) string {
	t.Helper()
	return "ws_" + randToken(t)
}

// Member seeds a REAL workspace_members row (the SEC-4 membership source, migration
// 0014) for (wsID, email) with role 'member', and returns the generated member id. This
// is what lets a cross-tenant test seed alice@/A and bob@/B and have the resolver
// distinguish them. Idempotent per (workspace_id, email).
func (d *DB) Member(t *testing.T, wsID, email string) string {
	t.Helper()
	memberID := "mbr_" + randToken(t)
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO workspace_members (workspace_id, email, role, member_id)
		 VALUES ($1, $2, 'member', $3)
		 ON CONFLICT (workspace_id, email) DO UPDATE SET member_id = EXCLUDED.member_id`,
		wsID, email, memberID); err != nil {
		t.Fatalf("testutil: seed workspace_member: %v", err)
	}
	return memberID
}

// Page persists a space and a page inside it (real rows) in wsID, authored by
// authorID, and returns the page id — the object a cross-tenant test fetches by id.
func (d *DB) Page(t *testing.T, wsID, authorID, title string) string {
	t.Helper()
	ctx := context.Background()
	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		wsID, "Space "+title, "space-"+randToken(t), authorID,
	).Scan(&spaceID); err != nil {
		t.Fatalf("testutil: seed space: %v", err)
	}
	var pageID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		spaceID, wsID, title, "page-"+randToken(t), authorID,
	).Scan(&pageID); err != nil {
		t.Fatalf("testutil: seed page: %v", err)
	}
	return pageID
}

func randToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("testutil: rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// safeIdent guards the database name interpolated into CREATE/DROP DATABASE — DDL
// identifiers cannot be parameterized, so the name (always our own generated
// "docs_test_<hex>", never user input) is validated then quoted via
// pgx.Identifier.Sanitize. Mirrors Track's testutil.
var safeIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func mustSafeIdent(name string) string {
	if !safeIdent.MatchString(name) {
		panic("testutil: refusing unsafe database identifier: " + name)
	}
	return name
}

func createDatabaseStmt(name string) string {
	return "CREATE DATABASE " + pgx.Identifier{mustSafeIdent(name)}.Sanitize()
}

func dropDatabaseStmt(name string) string {
	return "DROP DATABASE IF EXISTS " + pgx.Identifier{mustSafeIdent(name)}.Sanitize() + " WITH (FORCE)"
}
