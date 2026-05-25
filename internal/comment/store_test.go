package comment

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

func commentCols() []string {
	return []string{
		"id", "page_id", "block_id", "thread_id", "parent_id",
		"author_id", "author_name", "content",
		"resolved", "resolved_by", "resolved_at",
		"created_at", "updated_at",
	}
}

func ptrStr(s string) *string { return &s }

// ─── Create ──────────────────────────────────────────

func TestCreate_TopLevelInitsThreadIDToOwnID(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	// The store seeds thread_id = id by issuing INSERT and an
	// UPDATE in one round-trip via RETURNING. We model that as a
	// single QueryRow against the INSERT statement.
	pool.ExpectQuery(`INSERT INTO page_comments`).
		WithArgs("pg-1", (*string)(nil), "u-alice", "Alice", "first thought").
		WillReturnRows(pgxmock.NewRows(commentCols()).AddRow(
			"c-1", "pg-1", (*string)(nil), ptrStr("c-1"), (*string)(nil),
			"u-alice", "Alice", "first thought",
			false, (*string)(nil), (*time.Time)(nil),
			now, now,
		))

	c, err := store.Create(context.Background(), "pg-1", nil, "u-alice", "Alice", "first thought")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ThreadID == nil || *c.ThreadID != c.ID {
		t.Fatalf("expected thread_id=%q, got %v", c.ID, c.ThreadID)
	}
	if c.ParentID != nil {
		t.Fatalf("parent_id should be nil on top-level, got %v", *c.ParentID)
	}
}

func TestCreate_RejectsEmptyContent(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.Create(context.Background(), "pg-1", nil, "u-a", "A", "   ")
	if err == nil {
		t.Fatal("expected error on empty content")
	}
}

// ─── Reply ───────────────────────────────────────────

func TestReply_SharesThreadIDAndSetsParent(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	// 1. Lookup parent's thread_id + page_id.
	pool.ExpectQuery(`SELECT thread_id, page_id FROM page_comments WHERE id`).
		WithArgs("c-1").
		WillReturnRows(pgxmock.NewRows([]string{"thread_id", "page_id"}).
			AddRow(ptrStr("c-1"), "pg-1"))

	// 2. Insert the reply with the inherited thread_id.
	pool.ExpectQuery(`INSERT INTO page_comments`).
		WithArgs("pg-1", (*string)(nil), ptrStr("c-1"), ptrStr("c-1"),
			"u-bob", "Bob", "+1").
		WillReturnRows(pgxmock.NewRows(commentCols()).AddRow(
			"c-2", "pg-1", (*string)(nil), ptrStr("c-1"), ptrStr("c-1"),
			"u-bob", "Bob", "+1",
			false, (*string)(nil), (*time.Time)(nil),
			now, now,
		))

	c, err := store.Reply(context.Background(), "c-1", "u-bob", "Bob", "+1")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if c.ThreadID == nil || *c.ThreadID != "c-1" {
		t.Fatalf("reply thread_id wrong: %v", c.ThreadID)
	}
	if c.ParentID == nil || *c.ParentID != "c-1" {
		t.Fatalf("reply parent_id wrong: %v", c.ParentID)
	}
}

// ─── Resolve / Unresolve ─────────────────────────────

func TestResolve_AppliesToEntireThread(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`UPDATE page_comments SET resolved = true.*thread_id`).
		WithArgs("u-bob", "c-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	if err := store.Resolve(context.Background(), "c-1", "u-bob"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestUnresolve_ClearsThreadFields(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`UPDATE page_comments SET resolved = false`).
		WithArgs("c-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	if err := store.Unresolve(context.Background(), "c-1"); err != nil {
		t.Fatalf("Unresolve: %v", err)
	}
}

// ─── ListByPage ──────────────────────────────────────

func TestListByPage_NestsRepliesByThread(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	// One SELECT against the page returning rows in (thread,
	// position) order. The store buckets replies under their
	// parent in Go.
	pool.ExpectQuery(`SELECT.*FROM page_comments WHERE page_id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(commentCols()).
			// Top-level c-1 with two replies.
			AddRow("c-1", "pg-1", (*string)(nil), ptrStr("c-1"), (*string)(nil),
				"u-alice", "Alice", "top", false, (*string)(nil), (*time.Time)(nil), now, now).
			AddRow("c-2", "pg-1", (*string)(nil), ptrStr("c-1"), ptrStr("c-1"),
				"u-bob", "Bob", "reply 1", false, (*string)(nil), (*time.Time)(nil), now, now).
			AddRow("c-3", "pg-1", (*string)(nil), ptrStr("c-1"), ptrStr("c-1"),
				"u-carol", "Carol", "reply 2", false, (*string)(nil), (*time.Time)(nil), now, now).
			// Second top-level thread (no replies).
			AddRow("c-4", "pg-1", (*string)(nil), ptrStr("c-4"), (*string)(nil),
				"u-alice", "Alice", "another", false, (*string)(nil), (*time.Time)(nil), now, now))

	out, err := store.ListByPage(context.Background(), "pg-1", true)
	if err != nil {
		t.Fatalf("ListByPage: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 top-level, got %d", len(out))
	}
	if len(out[0].Replies) != 2 {
		t.Fatalf("first thread should have 2 replies, got %d", len(out[0].Replies))
	}
	if out[0].Replies[0].ID != "c-2" || out[0].Replies[1].ID != "c-3" {
		t.Fatalf("reply order wrong: %+v", out[0].Replies)
	}
	if len(out[1].Replies) != 0 {
		t.Fatalf("second thread should have 0 replies, got %d", len(out[1].Replies))
	}
}

func TestListByPage_FiltersResolvedWhenRequested(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM page_comments WHERE page_id.*resolved = false`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(commentCols()).
			AddRow("c-1", "pg-1", (*string)(nil), ptrStr("c-1"), (*string)(nil),
				"u-alice", "Alice", "open", false, (*string)(nil), (*time.Time)(nil), now, now))
	out, err := store.ListByPage(context.Background(), "pg-1", false)
	if err != nil {
		t.Fatalf("ListByPage open-only: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 open thread, got %d", len(out))
	}
}

// ─── GetStats ────────────────────────────────────────

func TestGetStats_CountsResolvedAndOpen(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT.*COUNT.*FROM page_comments WHERE page_id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows([]string{"total", "resolved"}).AddRow(int(10), int(7)))
	got, err := store.GetStats(context.Background(), "pg-1")
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if got.Total != 10 || got.Resolved != 7 || got.Open != 3 {
		t.Fatalf("unexpected stats: %+v", got)
	}
}

// ─── Delete ──────────────────────────────────────────

func TestDelete_AuthorOnly_Succeeds(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT author_id FROM page_comments WHERE id`).
		WithArgs("c-1").
		WillReturnRows(pgxmock.NewRows([]string{"author_id"}).AddRow("u-alice"))
	pool.ExpectExec(`DELETE FROM page_comments WHERE id`).
		WithArgs("c-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.Delete(context.Background(), "c-1", "u-alice"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDelete_RejectsNonAuthor(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT author_id FROM page_comments WHERE id`).
		WithArgs("c-1").
		WillReturnRows(pgxmock.NewRows([]string{"author_id"}).AddRow("u-alice"))
	err := store.Delete(context.Background(), "c-1", "u-bob")
	if err == nil {
		t.Fatal("expected forbidden")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "author") {
		t.Fatalf("error wrong: %v", err)
	}
}
