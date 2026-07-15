package migrate_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/talyvor/docs/internal/migrate"
	"github.com/talyvor/docs/internal/testutil"
	"github.com/talyvor/docs/migrations"
)

// Parity bar A. Docs had no migration runner: production applied migrations/ via
// Postgres's docker-entrypoint-initdb.d, which runs ONLY on first boot of an empty
// volume — so an existing deployment had no upgrade path at all, and nothing recorded
// which version a database was at. These tests drive the runner that replaces it.

func ctxT() context.Context { return context.Background() }

// FROM ZERO: an empty database gets every migration, in NNNN order, each recorded.
func TestApply_FromZero_AppliesAllAndRecordsEach(t *testing.T) {
	d := testutil.NewBlank(t)

	applied, err := migrate.Apply(ctxT(), d.Pool, migrations.FS)
	if err != nil {
		t.Fatalf("Apply from zero: %v", err)
	}

	// schema_migrations count must match the number of .sql files on disk.
	wantFiles, err := migrate.Files(migrations.FS)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(applied) != len(wantFiles) {
		t.Errorf("applied %d migrations, want %d (one per .sql file)", len(applied), len(wantFiles))
	}
	var recorded int
	if err := d.Pool.QueryRow(ctxT(), `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations (table must exist after Apply): %v", err)
	}
	if recorded != len(wantFiles) {
		t.Errorf("schema_migrations has %d rows, want %d (one per migration file)", recorded, len(wantFiles))
	}

	// The schema must actually be there — a runner that records without applying is
	// worse than none.
	for _, tbl := range []string{"spaces", "pages", "blocks", "workspace_members", "page_embeddings"} {
		var exists bool
		if err := d.Pool.QueryRow(ctxT(),
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, tbl).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", tbl, err)
		}
		if !exists {
			t.Errorf("table %q missing after Apply — recorded but not applied", tbl)
		}
	}

	// Versions must be recorded in NNNN order and match the filenames.
	rows, err := d.Pool.Query(ctxT(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	for i, f := range wantFiles {
		if i >= len(got) {
			break
		}
		if want := strings.SplitN(f, "_", 2)[0]; got[i] != want {
			t.Errorf("version[%d] = %q, want %q", i, got[i], want)
		}
	}
}

// RE-RUN IS A NO-OP: the second Apply must apply nothing.
func TestApply_SecondRunIsNoOp(t *testing.T) {
	d := testutil.NewBlank(t)

	first, err := migrate.Apply(ctxT(), d.Pool, migrations.FS)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first Apply applied nothing — fixture is wrong")
	}

	second, err := migrate.Apply(ctxT(), d.Pool, migrations.FS)
	if err != nil {
		t.Fatalf("second Apply must succeed: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second Apply applied %v, want [] (re-run must be a no-op)", second)
	}

	var recorded int
	if err := d.Pool.QueryRow(ctxT(), `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != len(first) {
		t.Errorf("schema_migrations has %d rows after re-run, want %d (no duplicate records)", recorded, len(first))
	}
}

// ADOPTION: a database already provisioned by the old initdb.d path has the full
// schema but NO schema_migrations. Apply must adopt it without error — the migrations
// are IF-NOT-EXISTS idempotent, so re-applying is safe — and record every version.
// This is the upgrade path for every existing deployment.
func TestApply_AdoptsInitdbProvisionedDatabase(t *testing.T) {
	d := testutil.New(t) // full schema applied the OLD way, no schema_migrations

	var exists bool
	if err := d.Pool.QueryRow(ctxT(),
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='schema_migrations')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("fixture wrong: schema_migrations already exists before Apply")
	}

	applied, err := migrate.Apply(ctxT(), d.Pool, migrations.FS)
	if err != nil {
		t.Fatalf("Apply must adopt an initdb.d-provisioned database (migrations are IF-NOT-EXISTS idempotent): %v", err)
	}
	files, _ := migrate.Files(migrations.FS)
	if len(applied) != len(files) {
		t.Errorf("adopted %d migrations, want %d", len(applied), len(files))
	}
	// And the pre-existing data must survive adoption.
	var spaces int
	if err := d.Pool.QueryRow(ctxT(), `SELECT count(*) FROM spaces`).Scan(&spaces); err != nil {
		t.Errorf("spaces table must survive adoption: %v", err)
	}
}

// CHECKSUM DRIFT → FAIL CLOSED. An already-applied migration whose bytes changed on
// disk means the database and the repo disagree about what schema is deployed. That
// must stop the boot, not be silently ignored.
func TestApply_ChecksumDrift_FailsClosed(t *testing.T) {
	d := testutil.NewBlank(t)

	orig := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS mig_a (id int);`)},
	}
	if _, err := migrate.Apply(ctxT(), d.Pool, orig); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	// Same version, different bytes — someone edited an applied migration.
	drifted := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS mig_a (id int, extra text);`)},
	}
	_, err := migrate.Apply(ctxT(), d.Pool, drifted)
	if err == nil {
		t.Fatal("Apply must FAIL on checksum drift of an applied migration, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "checksum") {
		t.Errorf("drift error should name the checksum, got: %v", err)
	}
}

// ORDERING: a malformed filename is a hard error, not a silently-skipped file. A
// migration that silently does not run is the failure mode this whole runner exists
// to prevent.
func TestFiles_RejectsMalformedName(t *testing.T) {
	bad := fstest.MapFS{
		"0001_ok.sql":     &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		"not-numbered.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	if _, err := migrate.Files(bad); err == nil {
		t.Fatal("Files must reject a .sql file that is not NNNN_name.sql, got nil")
	}
}

// ORDERING: duplicate version numbers are a hard error — this is the migration-number
// collision class that has bitten the sibling repos (two PRs both adding 0091).
func TestFiles_RejectsDuplicateVersion(t *testing.T) {
	dup := fstest.MapFS{
		"0001_one.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		"0001_two.sql": &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	if _, err := migrate.Files(dup); err == nil {
		t.Fatal("Files must reject duplicate version 0001, got nil")
	}
}

// ORDERING: files are returned in numeric NNNN order regardless of map iteration.
func TestFiles_NumericOrder(t *testing.T) {
	fs := fstest.MapFS{
		"0010_ten.sql":  &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		"0002_two.sql":  &fstest.MapFile{Data: []byte(`SELECT 1;`)},
		"0001_one.sql":  &fstest.MapFile{Data: []byte(`SELECT 1;`)},
	}
	got, err := migrate.Files(fs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0001_one.sql", "0002_two.sql", "0010_ten.sql"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// ATOMICITY: a migration that fails must not be recorded as applied — otherwise the
// next boot skips it and the schema is permanently wrong.
func TestApply_FailedMigrationIsNotRecorded(t *testing.T) {
	d := testutil.NewBlank(t)

	broken := fstest.MapFS{
		"0001_ok.sql":     &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS mig_ok (id int);`)},
		"0002_broken.sql": &fstest.MapFile{Data: []byte(`THIS IS NOT SQL;`)},
	}
	if _, err := migrate.Apply(ctxT(), d.Pool, broken); err == nil {
		t.Fatal("Apply must fail on a broken migration, got nil")
	}

	var recorded int
	if err := d.Pool.QueryRow(ctxT(),
		`SELECT count(*) FROM schema_migrations WHERE version = '0002'`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Errorf("broken migration 0002 was recorded as applied (%d rows) — the next boot would skip it", recorded)
	}
	// 0001 came before the failure and must have stuck.
	var ok int
	if err := d.Pool.QueryRow(ctxT(),
		`SELECT count(*) FROM schema_migrations WHERE version = '0001'`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok != 1 {
		t.Errorf("migration 0001 (applied before the failure) recorded %d times, want 1", ok)
	}
}

// A recorded migration whose file has vanished means the database is ahead of the
// repo — fail closed rather than pretend.
func TestApply_RecordedButMissingFile_FailsClosed(t *testing.T) {
	d := testutil.NewBlank(t)

	two := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS mig_a (id int);`)},
		"0002_b.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS mig_b (id int);`)},
	}
	if _, err := migrate.Apply(ctxT(), d.Pool, two); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	shrunk := fstest.MapFS{
		"0001_a.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE IF NOT EXISTS mig_a (id int);`)},
	}
	_, err := migrate.Apply(ctxT(), d.Pool, shrunk)
	if err == nil {
		t.Fatal("Apply must FAIL when the database has a version the repo no longer contains, got nil")
	}
}
