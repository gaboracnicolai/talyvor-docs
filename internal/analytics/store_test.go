package analytics

import (
	"context"
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

func TestGetWorkspaceStats_TopAndBottomPagesAndNeverRead(t *testing.T) {
	store, pool := newMockStore(t)

	// Aggregate totals.
	pool.ExpectQuery(`COUNT.*page_views.*workspace_id`).
		WithArgs("ws-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"total_views", "unique_viewers"}).
			AddRow(int(120), int(15)))

	// Most-read pages.
	pool.ExpectQuery(`(?i)page_id.*group by.*order by count.*desc`).
		WithArgs("ws-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "title", "view_count"}).
			AddRow("pg-1", "Top", int(50)).
			AddRow("pg-2", "Second", int(30)))

	// Least-read pages (with > 0 views).
	pool.ExpectQuery(`(?i)page_id.*group by.*order by count.*asc`).
		WithArgs("ws-1", 30).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "title", "view_count"}).
			AddRow("pg-9", "Cold", int(1)))

	// Never-read count.
	pool.ExpectQuery(`COUNT.*pages.*LEFT JOIN page_views`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"never"}).AddRow(int(3)))

	got, err := store.GetWorkspaceStats(context.Background(), "ws-1", 30)
	if err != nil {
		t.Fatalf("GetWorkspaceStats: %v", err)
	}
	if got.TotalViews != 120 {
		t.Fatalf("total views = %d", got.TotalViews)
	}
	if len(got.MostReadPages) != 2 || got.MostReadPages[0].PageID != "pg-1" {
		t.Fatalf("most read wrong: %+v", got.MostReadPages)
	}
	if len(got.LeastReadPages) != 1 || got.LeastReadPages[0].PageID != "pg-9" {
		t.Fatalf("least read wrong: %+v", got.LeastReadPages)
	}
	if got.NeverRead != 3 {
		t.Fatalf("never read = %d", got.NeverRead)
	}
}
