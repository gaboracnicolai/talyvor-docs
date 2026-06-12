package notification

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/talyvor/docs/internal/email"
)

func TestDeadLetterStore_RecordInsertsMetadata(t *testing.T) {
	pool := newMockPool(t)
	s := newDeadLetterStore(pool)
	// Only metadata is persisted — never the rendered body.
	pool.ExpectExec(`INSERT INTO notification_dead_letters`).
		WithArgs([]string{"a@b.c"}, "Review requested: Spec", 3, "smtp down").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Record(context.Background(),
		email.Message{To: []string{"a@b.c"}, Subject: "Review requested: Spec", HTMLBody: "<p>secret</p>"},
		3, "smtp down")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestDeadLetterStore_ListReturnsRows(t *testing.T) {
	pool := newMockPool(t)
	s := newDeadLetterStore(pool)
	when := time.Date(2026, 6, 12, 1, 0, 0, 0, time.UTC)
	pool.ExpectQuery(`SELECT .* FROM notification_dead_letters`).
		WithArgs(50).
		WillReturnRows(pgxmock.NewRows([]string{"id", "recipients", "subject", "attempts", "last_error", "created_at"}).
			AddRow(int64(7), []string{"a@b.c"}, "Review requested: Spec", 3, "smtp down", when))

	out, err := s.List(context.Background(), 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 row, got %d", len(out))
	}
	got := out[0]
	if got.ID != 7 || got.Subject != "Review requested: Spec" || got.Attempts != 3 || got.LastError != "smtp down" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

// Compile-time assertion that the store satisfies the queue's sink interface.
var _ email.DeadLetterSink = (*DeadLetterStore)(nil)
