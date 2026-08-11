package analytics

import (
	"context"
	"errors"
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
	// EVERY EXPECTATION IN THIS PACKAGE IS VERIFIED, NOT JUST THE ONES SOMEBODY REMEMBERED.
	//
	// 7 of this package's 9 expectations were unverified. This package is the
	// CONTRAST: control E7 deleted RecordView's INSERT and its test DID red — but by an ordered-
	// mock query MISMATCH on the next statement, not by anyone verifying the expectation. Being
	// caught and being checked are different things.
	//
	// pgxmock ignores an expectation that was never called unless you ASK it, and this package
	// asked PER TEST, where somebody remembered — which is the shape that leaves the next test
	// written uncovered. Measured by scripts/w31-partial-coverage-write-controls.py, family E.
	//
	// Registered AFTER pool.Close so it runs BEFORE it (t.Cleanup is LIFO). t.Errorf, not
	// t.Fatalf: a cleanup must not Goexit out of another cleanup.
	t.Cleanup(func() {
		if err := pool.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet or mismatched pgxmock expectations: %v", err)
		}
	})
	return newStore(pool), pool
}

func TestRecordView_InsertsRowAndIncrementsPageCounter(t *testing.T) {
	store, pool := newMockStore(t)

	// page_views INSERT + pages UPDATE in one transaction-equivalent
	// pair. RecordView fires them sequentially; mock both.
	pool.ExpectExec(`INSERT INTO page_views`).
		WithArgs("pg-1", "ws-1", "u-1", "Alice", 45).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`UPDATE pages SET view_count = view_count`).
		WithArgs("pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	err := store.RecordView(context.Background(), PageView{
		PageID:      "pg-1",
		WorkspaceID: "ws-1",
		ViewerID:    "u-1",
		ViewerName:  "Alice",
		Duration:    45,
	})
	if err != nil {
		t.Fatalf("RecordView: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRecordView_RejectsVeryShortViews(t *testing.T) {
	store, pool := newMockStore(t)
	// No DB expectations — the row must be dropped client of <3s.
	err := store.RecordView(context.Background(), PageView{
		PageID: "pg-1", WorkspaceID: "ws-1", Duration: 1,
	})
	if err != nil {
		t.Fatalf("RecordView short: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestGetReadStats_AggregatesViewsAndViewers(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	// Aggregate query: total views, unique viewers, avg duration, last viewed.
	pool.ExpectQuery(`COUNT.*page_views.*page_id`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"total_views", "unique_viewers", "avg_duration_sec", "last_viewed_at",
		}).AddRow(int(42), int(7), int(95), now))

	// Views by day.
	pool.ExpectQuery(`DATE_TRUNC.*FROM page_views`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"date", "count"}).
			AddRow(now.Truncate(24*time.Hour), int(5)).
			AddRow(now.Add(-24*time.Hour).Truncate(24*time.Hour), int(3)))

	// Top viewers.
	pool.ExpectQuery(`viewer_id.*page_views.*GROUP BY`).
		WithArgs("pg-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{
			"viewer_id", "viewer_name", "view_count", "last_viewed",
		}).
			AddRow("u-1", "Alice", int(12), now).
			AddRow("u-2", "Bob", int(8), now))

	got, err := store.GetReadStats(context.Background(), "pg-1", 30)
	if err != nil {
		t.Fatalf("GetReadStats: %v", err)
	}
	if got.TotalViews != 42 {
		t.Fatalf("total views = %d", got.TotalViews)
	}
	if got.UniqueViewers != 7 {
		t.Fatalf("unique viewers = %d", got.UniqueViewers)
	}
	if got.AvgDurationSec != 95 {
		t.Fatalf("avg duration = %d", got.AvgDurationSec)
	}
	if len(got.ViewsByDay) != 2 {
		t.Fatalf("days = %d, want 2", len(got.ViewsByDay))
	}
	if len(got.TopViewers) != 2 || got.TopViewers[0].ViewerID != "u-1" {
		t.Fatalf("top viewers wrong: %+v", got.TopViewers)
	}
}

// allowAllPages is a visibility gate that admits everything.
//
// ⚠ IT MAKES THIS TEST BLIND TO THE FILTER BY CONSTRUCTION, AND THAT IS THE POINT OF SAYING SO.
// With every page visible, a GetWorkspaceStats that dropped the AuthorizePageRead calls entirely
// would pass here. This test owns the PROJECTION — which statements run, with which arguments,
// and that their columns round-trip into the struct. The filter's catcher is
// privatespace_realpg_test.go, on real Postgres against the real permission engine. Neither
// subsumes the other; a uniform fixture cannot tell two sources apart.
type allowAllPages struct{}

func (allowAllPages) AuthorizePageRead(context.Context, string) (bool, bool) { return true, true }

func TestGetWorkspaceStats_TopAndBottomPagesAndNeverRead(t *testing.T) {
	store, pool := newMockStore(t)
	store.WithPageRead(allowAllPages{})

	// The whole ranked window — ONE statement for both cohorts, and NO `LIMIT` in it. The cap is
	// applied in Go after filtering, because a filter after a SQL LIMIT returns short lists.
	pool.ExpectQuery(`(?i)page_id.*group by.*order by count.*desc`).
		WithArgs("ws-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "title", "view_count"}).
			AddRow("pg-1", "Top", int(50)).
			AddRow("pg-2", "Second", int(30)).
			AddRow("pg-9", "Cold", int(1)))

	// Distinct viewers, restricted to the pages that survived the filter.
	pool.ExpectQuery(`(?i)count\(distinct viewer_id\).*page_id = any`).
		WithArgs("ws-1", 30, []string{"pg-1", "pg-2", "pg-9"}).
		WillReturnRows(pgxmock.NewRows([]string{"unique_viewers"}).AddRow(int(15)))

	// Never-read page IDS, not a COUNT — a count cannot be filtered.
	pool.ExpectQuery(`(?i)select p\.id from pages p.*left join page_views`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).
			AddRow("pg-a").AddRow("pg-b").AddRow("pg-c"))

	got, err := store.GetWorkspaceStats(context.Background(), "ws-1", 30)
	if err != nil {
		t.Fatalf("GetWorkspaceStats: %v", err)
	}
	// Summed from the surviving rows (50+30+1), never read off a workspace-wide COUNT.
	if got.TotalViews != 81 {
		t.Fatalf("total views = %d, want 81 (summed from the visible rows)", got.TotalViews)
	}
	if got.UniqueViewers != 15 {
		t.Fatalf("unique viewers = %d, want 15", got.UniqueViewers)
	}
	if len(got.MostReadPages) != 3 || got.MostReadPages[0].PageID != "pg-1" {
		t.Fatalf("most read wrong: %+v", got.MostReadPages)
	}
	// The tail of the same ranking, reversed — the set the old ORDER BY ASC statement returned.
	if len(got.LeastReadPages) != 3 || got.LeastReadPages[0].PageID != "pg-9" {
		t.Fatalf("least read wrong: %+v", got.LeastReadPages)
	}
	if got.NeverRead != 3 {
		t.Fatalf("never read = %d", got.NeverRead)
	}
}

// TestGetWorkspaceStats_NoGate_FailsClosed pins the misconfiguration answer. An unwired gate must
// not fall through to an unfiltered roll-up, and must not answer with a zeroed one either — on
// this surface zeroes are the positive claim "this workspace has no readership".
func TestGetWorkspaceStats_NoGate_FailsClosed(t *testing.T) {
	store, _ := newMockStore(t)

	got, err := store.GetWorkspaceStats(context.Background(), "ws-1", 30)
	if !errors.Is(err, ErrNoPageReadGate) {
		t.Fatalf("err = %v, want ErrNoPageReadGate", err)
	}
	if got != nil {
		t.Fatalf("stats = %+v, want nil — an unwired gate must not answer with numbers", got)
	}
}
