package database_test

// `MaxRows = 10_000` IS DECLARED, IS CITED AS THE REASON AN UNPAGINATED FETCH-EVERYTHING READ IS
// SAFE, AND IS ENFORCED NOWHERE.
//
// Two claims in this package rest on it, and both are written as statements of fact:
//
//	store.go:24  "MaxColumns + MaxRows match the spec constraints. Bounded so a runaway user
//	             (or agent) can't blow up a page render."
//	store.go:356 ListRows "fetches every row for the database, then applies the view's filters +
//	             sort in Go … because the row counts are bounded (MaxRows = 10K)"
//
// The second is load-bearing: it is the justification for reading the whole table into memory and
// filtering there instead of pushing predicates into SQL. A whole-repo census finds `MaxRows`
// mentioned exactly three times — its own declaration, that comment, and this file. `CreateRow`
// performs no count check of any kind.
//
// WHAT THIS TEST MEASURES, through the shipped store on real Postgres:
//
//	(1) the SHIPPED CreateRow accepts row number MaxRows+1 without error, and
//	(2) the SHIPPED ListRows then returns all MaxRows+1 of them — the unpaginated read is
//	    genuinely unpaginated, so the sentence that justifies it is false about its own premise.
//
// ⚠ THE CONTROL IS THE SIBLING IN THE SAME `const` BLOCK, AND IT IS WHAT MAKES THIS SPECIFIC.
// `MaxColumns = 50` IS enforced, and the control drives it through the SHIPPED `UpdateSchema` —
// the same kind of entry point `CreateRow` is — rather than the unexported validator, so the two
// halves are measured the same way. One of the two constants the comment names in a single breath
// is real and the other is not; the harness can plainly see a refusal when there is one.
//
// ⚠⚠ THIS TEST DOES NOT ASSERT THAT A CAP SHOULD EXIST, AND DELIBERATELY SO. Enforcing 10_000
// would introduce a hard limit where none has ever existed, which decides what happens to a
// customer who already holds more than 10_000 rows — a product call, not a plumbing one. This
// pins the MEASURED truth so the next reader does not re-derive it. If someone enforces the cap,
// (1) below goes RED: that is the intended signal to update this file and store.go:24/356
// together, not to loosen the assertion.

import (
	"context"
	"fmt"
	"testing"

	"github.com/talyvor/docs/internal/database"
	"github.com/talyvor/docs/internal/testutil"
)

func TestMaxRows_IsDeclaredCitedAndUnenforced_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	store := database.NewStore(d.Pool)

	W := d.Workspace(t)
	owner := d.Member(t, W, "maxrows-owner@corp.com")
	spaceID := seedSpaceX(t, d, W, owner, "MaxRowsSpace", false)
	pageID := seedPageX(t, d, W, spaceID, owner, "MaxRows page")
	dbID := seedDatabaseX(t, d, W, pageID, "big")

	// Seed exactly MaxRows rows straight through SQL. The point under test is the CAP, not the
	// insert path, and 10k round trips through CreateRow would buy nothing but wall clock.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO database_rows (database_id, values, position)
         SELECT $1, '{}'::jsonb, i FROM generate_series(1, $2) AS i`,
		dbID, database.MaxRows,
	); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
	var seeded int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM database_rows WHERE database_id = $1`, dbID).Scan(&seeded); err != nil {
		t.Fatalf("count: %v", err)
	}
	if seeded != database.MaxRows {
		t.Fatalf("seeded %d rows, want exactly MaxRows=%d — the measurement below would be about "+
			"the wrong boundary", seeded, database.MaxRows)
	}

	// (1) The SHIPPED create path, at the row that is one past the declared ceiling.
	over, err := store.CreateRow(ctx, database.Row{DatabaseID: dbID, Values: map[string]any{"c-1": "over"}, Position: float64(database.MaxRows + 1)}, []string{W})
	if err != nil {
		t.Errorf("(1) CreateRow refused row %d with %v — MaxRows is now ENFORCED. That is a change "+
			"in product behaviour, not a test failure: update store.go:24 and store.go:356, whose "+
			"prose already claims this bound holds, and then delete this test.", database.MaxRows+1, err)
	} else if over == nil {
		t.Errorf("(1) CreateRow returned (nil, nil) past MaxRows")
	} else {
		t.Logf("MEASURED: CreateRow accepted row %d of a declared %d-row maximum, no error, id=%s",
			database.MaxRows+1, database.MaxRows, over.ID)
	}

	// (2) The SHIPPED read the comment calls safe. No view, so no filtering — just the fetch.
	got, err := store.ListRows(ctx, dbID, nil, []string{W})
	if err != nil {
		t.Fatalf("(2) ListRows: %v", err)
	}
	if len(got) != database.MaxRows+1 {
		t.Errorf("(2) ListRows returned %d rows, want %d — if this is now CAPPED the read is no "+
			"longer unpaginated and store.go:356's justification has changed", len(got), database.MaxRows+1)
	} else {
		t.Logf("MEASURED: ListRows fetched all %d rows into memory in one unpaginated read — "+
			"store.go:356 justifies that with a bound (MaxRows=%d) that nothing enforces",
			len(got), database.MaxRows)
	}

	// [SIBLING-CONTROL] the other constant the same comment names IS enforced, so a refusal is
	// something this harness can see and the absence above is specific to MaxRows.
	cols := make([]database.ColumnDef, database.MaxColumns+1)
	for i := range cols {
		cols[i] = database.ColumnDef{ID: fmt.Sprintf("c-%d", i), Name: fmt.Sprintf("C%d", i), Type: "text"}
	}
	if _, err := store.UpdateSchema(ctx, dbID, cols, []string{W}); err == nil {
		t.Errorf("[SIBLING-CONTROL] UpdateSchema accepted %d columns past MaxColumns=%d — the control "+
			"constant is NOT enforced either, so (1) says nothing specific about MaxRows", len(cols), database.MaxColumns)
	} else {
		t.Logf("[SIBLING-CONTROL] UpdateSchema refused %d columns: %v — a declared bound in this same "+
			"const block IS enforced through a shipped entry point, so the absence measured above is "+
			"specific to MaxRows", len(cols), err)
	}
}
