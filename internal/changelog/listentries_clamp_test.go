package changelog

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

// listentries_clamp_test.go — THE SAME FOUR-LINE CLAMP, TWICE IN THIS FILE, ONE OF THEM
// WATCHED.
//
// store.go declares an identical `if limit <= 0 { 20 }; if limit > 100 { 100 }` block in
// GetPublicFeed and in ListEntries. TestGetPublicFeed_ClampsLimit defends the first.
//
// ⚠ MEASURED (W3.53/W3.57, tab-k4m7, ~/talyvor-queue/w353-docs-reach-k4m7.py arm F6): remove
// the clamp from ListEntries and THE WHOLE SUITE STAYS GREEN, on every run. Two identical
// guards, one of which is watched, is the most confusing state to leave a reader in — it reads
// as covered because its twin is.
//
// ⚠⚠ THIS IS THE SHAPE THE SAME CENSUS FOUND IN FIVE REPOSITORIES IN ONE NIGHT: the defended
// and undefended cases are ADJACENT — same file, same constant, same idea. A test gets written
// for the case somebody was thinking about, and the identical case a few hundred lines down is
// invisible precisely because it looks already handled. Reading finds none of them; a one-line
// mutation finds all of them.
//
// ⚠ AND THE TWIN IS THE MODEL, NOT THE AUTHORITY. TestGetPublicFeed_ClampsLimit asserts only
// that 9999 becomes 100. That is the half which cannot fail on a clamp that pins everything to
// 1 — so this file also pins the value at the bound, the default, and the offset correction
// that sits in the same block. Copying the twin's coverage would have reproduced its gap.

func TestListEntries_ClampsAndDefaults(t *testing.T) {
	const (
		wantDefault = 20
		wantMax     = 100
	)
	cases := []struct {
		name       string
		limit      int
		offset     int
		wantLimit  int
		wantOffset int
	}{
		{"absent limit takes the default", 0, 0, wantDefault, 0},
		{"negative limit takes the default, it does not reach SQL", -5, 0, wantDefault, 0},
		{"a limit under the cap is honoured", 7, 0, 7, 0},
		{"exactly the cap is honoured", 100, 0, wantMax, 0},
		{"one over the cap is clamped", 101, 0, wantMax, 0},
		{"absurdly over the cap is clamped", 9999, 0, wantMax, 0},
		{"a negative offset is corrected, it does not reach SQL", 10, -7, 10, 0},
		{"an ordinary offset is passed through", 10, 40, 10, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, pool := newMockStore(t, &fakeTrack{})
			// The no-entryType branch: LIMIT is $3 and OFFSET is $4. Both are asserted, so a
			// clamp that fixed the limit while letting a negative offset through would fail.
			pool.ExpectQuery(`FROM changelog_entries\s+WHERE page_id = \$1 AND workspace_id = ANY`).
				WithArgs("pg-1", []string{"ws-1"}, c.wantLimit, c.wantOffset).
				WillReturnRows(pgxmock.NewRows(entryCols()))

			if _, err := store.ListEntries(context.Background(), "pg-1", nil, c.limit, c.offset,
				[]string{"ws-1"}); err != nil {
				t.Fatalf("ListEntries(limit=%d, offset=%d): %v", c.limit, c.offset, err)
			}
			// newMockStore verifies expectations in a cleanup, so an argument that did not
			// match fails here rather than passing quietly.
		})
	}
}

// TestListEntries_ClampsTheTypedBranchToo covers the OTHER query in the same function. The
// clamp runs before the branch, so this cannot diverge today — which is exactly why it is
// worth an assertion: the two branches build their SQL separately, and a future edit that
// moved the clamp inside one of them would leave the other unbounded with nothing to say so.
func TestListEntries_ClampsTheTypedBranchToo(t *testing.T) {
	store, pool := newMockStore(t, &fakeTrack{})
	et := EntryImprovement
	pool.ExpectQuery(`FROM changelog_entries\s+WHERE page_id = \$1 AND type = \$2 AND workspace_id = ANY`).
		WithArgs("pg-1", string(et), []string{"ws-1"}, 100, 0).
		WillReturnRows(pgxmock.NewRows(entryCols()))

	if _, err := store.ListEntries(context.Background(), "pg-1", &et, 9999, -3,
		[]string{"ws-1"}); err != nil {
		t.Fatalf("ListEntries (typed): %v", err)
	}
}
