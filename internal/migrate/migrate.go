// Package migrate is Docs's schema migration runner.
//
// Before this package, production applied migrations/ via Postgres's
// docker-entrypoint-initdb.d, which runs ONLY on first boot of an empty data
// directory. That meant an existing deployment had no upgrade path at all — dropping
// a new 00NN_*.sql on a running install did nothing, silently — and nothing recorded
// which schema version a database was actually at.
//
// This runner replaces that mechanism: the same embedded *.sql files, applied in NNNN
// order, each recorded in schema_migrations with its checksum. It is the minimal
// mirror of talyvor-track's embedded-FS approach.
//
// Guards, all fail-closed:
//   - ORDERING   — every .sql must be NNNN_name.sql; duplicate versions are rejected
//     (the migration-number collision class that has bitten the sibling repos).
//   - CHECKSUM   — an applied migration whose bytes changed on disk means the database
//     and the repo disagree about the deployed schema. Hard error.
//   - COMPLETENESS — a version recorded in the database but absent from the repo means
//     the database is ahead of the code. Hard error.
//   - ATOMICITY  — each migration and its schema_migrations record commit in ONE
//     transaction, so a failed migration is never recorded as applied.
//   - CONCURRENCY — a session-level advisory lock serialises concurrent boots, so N
//     replicas starting together cannot race each other's applies.
//
// The migrations are written IF-NOT-EXISTS idempotent, which is what lets this runner
// ADOPT a database provisioned the old initdb.d way: re-applying is a no-op against
// the existing objects, and the versions get recorded.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockKey is a fixed, arbitrary key namespaced to this runner. Any process
// applying Docs migrations takes the same lock, so concurrent boots serialise instead
// of racing. Chosen once; changing it would defeat the purpose.
const advisoryLockKey int64 = 7_312_004_119_540_021

const createTableSQL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// fileRe matches NNNN_snake_name.sql. The 4-digit version is the ordering key and the
// schema_migrations primary key.
var fileRe = regexp.MustCompile(`^(\d{4})_([a-zA-Z0-9_]+)\.sql$`)

// Record is one applied migration as recorded in schema_migrations.
type Record struct {
	Version  string
	Name     string
	Checksum string
}

// Files returns every migration filename in fsys in numeric NNNN order, after
// validating the whole set. A malformed name or a duplicate version is an error, not a
// skip: a migration that silently does not run is precisely the failure this runner
// exists to prevent.
func Files(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migrations dir: %w", err)
	}
	var files []string
	seen := map[string]string{} // version → filename
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrate: %q is not a valid migration name (want NNNN_name.sql)", e.Name())
		}
		if prev, dup := seen[m[1]]; dup {
			return nil, fmt.Errorf("migrate: duplicate migration version %s: %q and %q", m[1], prev, e.Name())
		}
		seen[m[1]] = e.Name()
		files = append(files, e.Name())
	}
	// NNNN is zero-padded and fixed-width, so lexicographic == numeric.
	sort.Strings(files)
	return files, nil
}

// versionOf / nameOf split a validated filename. Callers must have gone through Files.
func versionOf(f string) string { return fileRe.FindStringSubmatch(f)[1] }
func nameOf(f string) string    { return fileRe.FindStringSubmatch(f)[2] }

func checksumOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Applied returns the migrations recorded in schema_migrations, keyed by version.
// An absent table is not an error — it means nothing has been applied yet.
func Applied(ctx context.Context, pool *pgxpool.Pool) (map[string]Record, error) {
	out := map[string]Record{}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'schema_migrations')`,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("migrate: probe schema_migrations: %w", err)
	}
	if !exists {
		return out, nil
	}
	rows, err := pool.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Version, &r.Name, &r.Checksum); err != nil {
			return nil, err
		}
		out[r.Version] = r
	}
	return out, rows.Err()
}

// Apply brings the database up to date with fsys and returns the versions it applied
// (empty when already current — a re-run is a no-op). Every guard in the package doc
// is enforced here; any violation returns an error and applies nothing further.
func Apply(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	files, err := Files(fsys)
	if err != nil {
		return nil, err
	}

	// One connection for the whole run: an advisory lock is session-scoped, so it must
	// be taken and released on the same conn that does the work.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire conn: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return nil, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer func() {
		// Best-effort unlock; releasing the conn would drop the session lock anyway.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if _, err := conn.Exec(ctx, createTableSQL); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied, err := Applied(ctx, pool)
	if err != nil {
		return nil, err
	}

	// COMPLETENESS: the database must not know versions the repo has lost.
	known := map[string]bool{}
	for _, f := range files {
		known[versionOf(f)] = true
	}
	for v := range applied {
		if !known[v] {
			return nil, fmt.Errorf("migrate: database has migration %s applied but the repo has no such file — "+
				"the database is ahead of this build; refusing to proceed", v)
		}
	}

	var out []string
	for _, f := range files {
		version, name := versionOf(f), nameOf(f)
		body, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", f, err)
		}
		sum := checksumOf(body)

		if rec, ok := applied[version]; ok {
			// CHECKSUM: an applied migration must not have changed on disk.
			if rec.Checksum != sum {
				return nil, fmt.Errorf("migrate: checksum mismatch for migration %s (%s): "+
					"recorded %s, file is now %s — an already-applied migration was edited; "+
					"add a new migration instead of changing history",
					version, f, rec.Checksum[:12], sum[:12])
			}
			continue // already applied, unchanged → skip
		}

		// ATOMICITY: the DDL and its record commit together, so a failure can never
		// leave a migration recorded-but-not-applied (which the next boot would skip).
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("migrate: begin %s: %w", f, err)
		}
		// No args → pgx uses the simple protocol, so a file may contain multiple
		// statements. This is the same execution shape initdb.d used.
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("migrate: apply %s: %w", f, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			version, name, sum,
		); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("migrate: record %s: %w", f, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("migrate: commit %s: %w", f, err)
		}
		out = append(out, version)
	}
	return out, nil
}
