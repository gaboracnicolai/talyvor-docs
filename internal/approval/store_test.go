package approval

import (
	"context"
	"strings"
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

func testRequestCols() []string {
	return []string{
		"id", "page_id", "workspace_id", "requested_by", "reviewers",
		"message", "due_date", "status", "created_at", "updated_at",
	}
}

func testDecisionCols() []string {
	return []string{"id", "request_id", "reviewer_id", "decision", "comment", "created_at"}
}

// ─── RequestApproval ──────────────────────────────────

func TestRequestApproval_CreatesRequestDecisionsAndFlipsPage(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	due := now.Add(24 * time.Hour)
	reviewers := []string{"u-alice", "u-bob"}

	// 1. Verify the page exists.
	pool.ExpectQuery(`SELECT 1 FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))

	// 2. Insert approval_requests row.
	pool.ExpectQuery(`INSERT INTO approval_requests`).
		WithArgs("pg-1", "ws-1", "u-author", reviewers, "please review", &due).
		WillReturnRows(pgxmock.NewRows(testRequestCols()).AddRow(
			"req-1", "pg-1", "ws-1", "u-author", reviewers,
			"please review", &due, "pending", now, now,
		))

	// 3. One decision per reviewer (pending).
	for _, r := range reviewers {
		pool.ExpectExec(`INSERT INTO review_decisions`).
			WithArgs("req-1", r).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}

	// 4. Flip pages.doc_status to in_review.
	pool.ExpectExec(`UPDATE pages SET doc_status`).
		WithArgs("in_review", "pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	got, err := store.RequestApproval(context.Background(),
		"pg-1", "ws-1", "u-author", reviewers, "please review", &due)
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if got.ID != "req-1" || got.Status != ApprovalPending {
		t.Fatalf("unexpected: %+v", got)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRequestApproval_RequiresAtLeastOneReviewer(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.RequestApproval(context.Background(), "pg-1", "ws-1", "u-author", nil, "", nil)
	if err == nil {
		t.Fatal("expected error with empty reviewers")
	}
}

func TestRequestApproval_RejectsMissingPage(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT 1 FROM pages WHERE id`).
		WithArgs("pg-missing").
		WillReturnError(pgxNoRows{})
	_, err := store.RequestApproval(context.Background(),
		"pg-missing", "ws-1", "u-author", []string{"u-a"}, "", nil)
	if err == nil {
		t.Fatal("expected error for missing page")
	}
}

// ─── Decide ───────────────────────────────────────────

func TestDecide_AllApproved_FlipsRequestAndPageToApproved(t *testing.T) {
	store, pool := newMockStore(t)

	// Update this reviewer's decision.
	pool.ExpectExec(`UPDATE review_decisions SET decision`).
		WithArgs("approved", "lgtm", "req-1", "u-alice").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// Look up the aggregate state of all decisions.
	pool.ExpectQuery(`SELECT decision FROM review_decisions WHERE request_id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).
			AddRow("approved").
			AddRow("approved"))

	// Update the request status + flip the page.
	pool.ExpectQuery(`SELECT page_id FROM approval_requests WHERE id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"page_id"}).AddRow("pg-1"))
	pool.ExpectExec(`UPDATE approval_requests SET status`).
		WithArgs("approved", "req-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(`UPDATE pages SET doc_status`).
		WithArgs("approved", "pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := store.Decide(context.Background(), "req-1", "u-alice", "approved", "lgtm"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDecide_AnyRejected_FlipsToRejected(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectExec(`UPDATE review_decisions SET decision`).
		WithArgs("rejected", "missing edge case", "req-1", "u-bob").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// Aggregate: one rejection wins.
	pool.ExpectQuery(`SELECT decision FROM review_decisions WHERE request_id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).
			AddRow("approved").
			AddRow("rejected"))

	pool.ExpectQuery(`SELECT page_id FROM approval_requests WHERE id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"page_id"}).AddRow("pg-1"))
	pool.ExpectExec(`UPDATE approval_requests SET status`).
		WithArgs("rejected", "req-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	pool.ExpectExec(`UPDATE pages SET doc_status`).
		WithArgs("rejected", "pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := store.Decide(context.Background(), "req-1", "u-bob", "rejected", "missing edge case"); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDecide_StillPending_LeavesRequestStatusAlone(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectExec(`UPDATE review_decisions SET decision`).
		WithArgs("approved", "", "req-1", "u-alice").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// One approved + one still pending → no aggregate flip yet.
	pool.ExpectQuery(`SELECT decision FROM review_decisions WHERE request_id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows([]string{"decision"}).
			AddRow("approved").
			AddRow("pending"))
	// NOTE: no UPDATE on approval_requests or pages — the second
	// reviewer hasn't decided yet.

	if err := store.Decide(context.Background(), "req-1", "u-alice", "approved", ""); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDecide_RejectsUnknownDecisionValue(t *testing.T) {
	store, _ := newMockStore(t)
	if err := store.Decide(context.Background(), "req-1", "u-a", "maybe", ""); err == nil {
		t.Fatal("expected error on invalid decision")
	}
}

// ─── ListPending ──────────────────────────────────────

func TestListPending_FiltersByReviewerWithPendingDecisions(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`FROM approval_requests.*JOIN review_decisions.*reviewer_id`).
		WithArgs("u-alice", "ws-1").
		WillReturnRows(pgxmock.NewRows(testRequestCols()).
			AddRow("req-1", "pg-1", "ws-1", "u-author", []string{"u-alice"},
				"please look", (*time.Time)(nil), "pending", now, now))

	out, err := store.ListPending(context.Background(), "u-alice", "ws-1")
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(out) != 1 || out[0].ID != "req-1" {
		t.Fatalf("unexpected: %+v", out)
	}
}

// ─── PublishApproved ──────────────────────────────────

func TestPublishApproved_SucceedsWhenApproved(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT doc_status FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows([]string{"doc_status"}).AddRow("approved"))
	pool.ExpectExec(`UPDATE pages SET doc_status`).
		WithArgs("approved", "pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	if err := store.PublishApproved(context.Background(), "pg-1"); err != nil {
		t.Fatalf("PublishApproved: %v", err)
	}
}

func TestPublishApproved_FailsWhenStatusNotApproved(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT doc_status FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows([]string{"doc_status"}).AddRow("draft"))
	err := store.PublishApproved(context.Background(), "pg-1")
	if err == nil {
		t.Fatal("expected error when status != approved")
	}
	if !strings.Contains(err.Error(), "approved") {
		t.Fatalf("error should mention approval state: %v", err)
	}
}

// ─── GetDecisions ─────────────────────────────────────

func TestGetDecisions_ReturnsAllForRequest(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM review_decisions WHERE request_id`).
		WithArgs("req-1").
		WillReturnRows(pgxmock.NewRows(testDecisionCols()).
			AddRow("d-1", "req-1", "u-alice", "approved", "lgtm", now).
			AddRow("d-2", "req-1", "u-bob", "pending", "", now))

	out, err := store.GetDecisions(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("GetDecisions: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2, got %d", len(out))
	}
}

// pgxNoRows mimics pgx.ErrNoRows so the store can detect missing
// pages via the QueryRow scan path.
type pgxNoRows struct{}

func (pgxNoRows) Error() string { return "no rows in result set" }
