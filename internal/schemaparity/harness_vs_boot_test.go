// Package schemaparity holds the guard that keeps the TEST harness's schema equal to the schema a
// DEPLOYMENT gets.
//
// THE CLASS. `testutil.New` is what all 78 real-Postgres tests in this repository run against —
// every tenancy, IDOR, cross-workspace and authz guard among them. It builds its schema by reading
// the embedded `.sql` files and `pool.Exec`-ing each one directly (`applyMigrations`,
// harness.go:154). A deployment does not: `cmd/docs` calls `migrate.Apply`, which owns the
// ordering, the checksum ledger and the version gate. THE TWO ARE DIFFERENT CODE PATHS TO THE SAME
// INTENT, and nothing compared their results.
//
// ⚠ MEASURED, WHICH IS HOW THIS FILE CAME TO EXIST: they differ today by exactly the migration
// ledger. Harness 202 columns / 66 indexes; boot 206 / 67; the delta is `schema_migrations`'s four
// columns and its primary key, and nothing else — every other column, type, nullability, default,
// index and constraint is byte for byte. That is the reassuring answer, and it is precisely why it
// needs a guard: it is only reassuring while it stays true, and a migration that starts relying on
// something migrate.Apply does and applyMigrations does not would make every green in this
// repository a statement about a database no customer has.
//
// It was found because W3.18's sqlguard reported `migrate.go`'s own two statements as findings
// against the harness. They were not findings. The harness was a different database.
//
// WHAT THIS ASSERTS:
//
//	S1 COLUMNS      every column — name, type, nullability, default — is identical, except the
//	                ledger's, which are named ONE BY ONE rather than filtered by a pattern.
//	S2 INDEXES      every index definition is identical, same exception.
//	S3 CONSTRAINTS  every table constraint (primary key, unique, foreign key, check) is identical.
//	S4 LEDGER       `schema_migrations` really is present at boot and absent in the harness. This
//	                is the rule that keeps S1-S3's exception HONEST: if the harness ever starts
//	                creating the ledger, the exception must be deleted, not left to silently
//	                exempt a table that is now in both.
//	S5 FLOOR        both schemas are non-trivial. Comparing two empty databases succeeds.
package schemaparity

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/migrate"
	"github.com/talyvor/docs/internal/testutil"
	"github.com/talyvor/docs/migrations"
)

// ledgerTable is the ONE table the two paths are allowed to disagree about, and it is named as a
// literal rather than matched by a prefix. A pattern like "anything starting with schema_" would
// have exempted a real divergence the day somebody added `schema_cache`.
const ledgerTable = "schema_migrations"

// floors are the sizes measured at c460d83. They are FLOORS: a comparison between two schemas that
// have both become empty passes every equality rule in this file and proves nothing.
const (
	columnFloor = 190
	indexFloor  = 60
)

func query(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

const columnsSQL = `
SELECT table_name||'.'||column_name||' :: '||data_type
       ||' null='||is_nullable||' default='||coalesce(column_default,'-')
  FROM information_schema.columns WHERE table_schema='public'`

const indexesSQL = `
SELECT tablename||' :: '||indexdef FROM pg_indexes WHERE schemaname='public'`

// Constraint definitions come from pg_constraint rather than information_schema because
// information_schema.table_constraints does not carry the DEFINITION — two different CHECKs with
// system-generated names would compare equal there, which is the shape a guard is asked to catch.
const constraintsSQL = `
SELECT rel.relname||' :: '||con.conname||' :: '||pg_get_constraintdef(con.oid)
  FROM pg_constraint con
  JOIN pg_class rel ON rel.oid = con.conrelid
  JOIN pg_namespace ns ON ns.oid = rel.relnamespace
 WHERE ns.nspname = 'public'`

// dropLedger removes the rows that belong to the migration ledger, and returns how many it
// removed so the caller can assert the exception did something. An exception that silently
// matches nothing is indistinguishable from one that is no longer needed.
func dropLedger(rows []string) (kept []string, removed int) {
	for _, r := range rows {
		if strings.HasPrefix(r, ledgerTable+".") || strings.HasPrefix(r, ledgerTable+" ::") {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	return kept, removed
}

func diff(t *testing.T, rule, label string, harness, boot []string) {
	t.Helper()
	inBoot := map[string]bool{}
	for _, s := range boot {
		inBoot[s] = true
	}
	inHarness := map[string]bool{}
	for _, s := range harness {
		inHarness[s] = true
	}
	var onlyHarness, onlyBoot []string
	for _, s := range harness {
		if !inBoot[s] {
			onlyHarness = append(onlyHarness, s)
		}
	}
	for _, s := range boot {
		if !inHarness[s] {
			onlyBoot = append(onlyBoot, s)
		}
	}
	for _, s := range onlyHarness {
		t.Errorf("%s: %s exists in the TEST harness and not in a deployment: %s\n"+
			"    Every real-PG test in this repository runs against the harness. A column, index "+
			"or constraint only it has makes those tests statements about a database no customer "+
			"has.", rule, label, s)
	}
	for _, s := range onlyBoot {
		t.Errorf("%s: %s exists in a DEPLOYMENT and not in the test harness: %s\n"+
			"    Nothing in the suite can exercise it, so a defect that needs it is invisible "+
			"until production.", rule, label, s)
	}
}

// schemas builds both databases: the harness's, and the one cmd/docs boots into.
func schemas(t *testing.T) (harness, boot *pgxpool.Pool) {
	t.Helper()
	h := testutil.New(t)
	b := testutil.NewBlank(t)
	applied, err := migrate.Apply(context.Background(), b.Pool, migrations.FS)
	if err != nil {
		t.Fatalf("migrate.Apply (the boot path) failed: %v", err)
	}
	if len(applied) < 20 {
		t.Fatalf("S5: migrate.Apply reported %d migrations; the 'boot' schema is not one", len(applied))
	}
	return h.Pool, b.Pool
}

func TestHarnessSchemaEqualsBootSchema(t *testing.T) {
	h, b := schemas(t)

	hCols, bCols := query(t, h, columnsSQL), query(t, b, columnsSQL)
	// S5 FLOOR first: every equality rule below is vacuous on two empty databases.
	if len(hCols) < columnFloor || len(bCols) < columnFloor {
		t.Fatalf("S5: harness has %d columns and boot has %d; the floor is %d. Two schemas that "+
			"have both become empty satisfy every equality rule in this file",
			len(hCols), len(bCols), columnFloor)
	}

	// S4 LEDGER — the exception must be REAL in both directions before it is applied. If the
	// harness starts creating the ledger, the exception is stale and must be deleted rather than
	// left to exempt a table that is now in both.
	hKeptCols, hDropped := dropLedger(hCols)
	bKeptCols, bDropped := dropLedger(bCols)
	if hDropped != 0 {
		t.Errorf("S4: the TEST harness now creates %s (%d columns). That is a good change and it "+
			"makes this file's one exception stale — delete ledgerTable and the dropLedger calls "+
			"rather than leaving an exemption that now hides a table present in both",
			ledgerTable, hDropped)
	}
	if bDropped == 0 {
		t.Errorf("S4: a deployment no longer creates %s. Either migrate.Apply stopped recording "+
			"what it applied — which is a real finding — or the ledger moved, and this file's "+
			"exception is now exempting nothing while looking maintained", ledgerTable)
	}

	// S1 COLUMNS
	diff(t, "S1", "column", hKeptCols, bKeptCols)

	// S2 INDEXES
	hIdx, bIdx := query(t, h, indexesSQL), query(t, b, indexesSQL)
	if len(hIdx) < indexFloor || len(bIdx) < indexFloor {
		t.Fatalf("S5: harness has %d indexes and boot has %d; the floor is %d", len(hIdx), len(bIdx), indexFloor)
	}
	hKeptIdx, _ := dropLedger(hIdx)
	bKeptIdx, _ := dropLedger(bIdx)
	diff(t, "S2", "index", hKeptIdx, bKeptIdx)

	// S3 CONSTRAINTS
	hCon, bCon := query(t, h, constraintsSQL), query(t, b, constraintsSQL)
	hKeptCon, _ := dropLedger(hCon)
	bKeptCon, _ := dropLedger(bCon)
	diff(t, "S3", "constraint", hKeptCon, bKeptCon)

	t.Logf("compared %d columns, %d indexes, %d constraints (ledger excepted: %d boot columns)",
		len(hKeptCols), len(hKeptIdx), len(hKeptCon), bDropped)
}
