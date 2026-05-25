package pagelock

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

func lockCols() []string {
	return []string{"locked", "locked_by", "locked_at", "doc_status"}
}

func ptrStr(s string) *string { return &s }

// ─── Lock ────────────────────────────────────────────

func TestLock_SetsStateAndRecordsLocker(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	// Current state — page is unlocked.
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(false, (*string)(nil), (*time.Time)(nil), "draft"))

	// Write the lock.
	pool.ExpectQuery(`UPDATE pages SET locked = true`).
		WithArgs("u-alice", "pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))

	state, err := store.Lock(context.Background(), "pg-1", "u-alice")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if !state.Locked || state.LockedBy == nil || *state.LockedBy != "u-alice" {
		t.Fatalf("unexpected: %+v", state)
	}
}

func TestLock_IdempotentForSameUser(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC().Add(-time.Hour)

	// Already locked by u-alice — Lock by u-alice should just return
	// the existing state without writing again.
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))

	state, err := store.Lock(context.Background(), "pg-1", "u-alice")
	if err != nil {
		t.Fatalf("Lock idempotent: %v", err)
	}
	if !state.Locked || state.LockedBy == nil || *state.LockedBy != "u-alice" {
		t.Fatalf("expected unchanged state, got %+v", state)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestLock_RejectsWhenLockedByOther(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC().Add(-time.Hour)

	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))

	_, err := store.Lock(context.Background(), "pg-1", "u-bob")
	if err == nil {
		t.Fatal("expected conflict")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "locked") {
		t.Fatalf("error should mention lock state: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── Unlock ──────────────────────────────────────────

func TestUnlock_ByLockerSucceeds(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))
	pool.ExpectExec(`UPDATE pages SET locked = false`).
		WithArgs("pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := store.Unlock(context.Background(), "pg-1", "u-alice", false); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestUnlock_ByAdminSucceedsEvenIfNotLocker(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))
	pool.ExpectExec(`UPDATE pages SET locked = false`).
		WithArgs("pg-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if err := store.Unlock(context.Background(), "pg-1", "u-bob", true); err != nil {
		t.Fatalf("Unlock admin: %v", err)
	}
}

func TestUnlock_ByNonLockerNonAdminFails(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))

	if err := store.Unlock(context.Background(), "pg-1", "u-bob", false); err == nil {
		t.Fatal("expected forbidden")
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ─── CanEdit ─────────────────────────────────────────

func TestCanEdit_TrueWhenNotLocked(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(false, (*string)(nil), (*time.Time)(nil), "draft"))
	ok, reason, err := store.CanEdit(context.Background(), "pg-1", "u-bob", false)
	if err != nil {
		t.Fatalf("CanEdit: %v", err)
	}
	if !ok {
		t.Fatalf("expected true, reason=%q", reason)
	}
}

func TestCanEdit_FalseWhenLockedByOther(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))
	ok, reason, _ := store.CanEdit(context.Background(), "pg-1", "u-bob", false)
	if ok {
		t.Fatal("expected false")
	}
	if !strings.Contains(strings.ToLower(reason), "locked") {
		t.Fatalf("reason wrong: %q", reason)
	}
}

func TestCanEdit_TrueWhenLockedBySelf(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))
	ok, _, _ := store.CanEdit(context.Background(), "pg-1", "u-alice", false)
	if !ok {
		t.Fatal("locker should always be able to edit their own lock")
	}
}

func TestCanEdit_TrueForAdminEvenWhenLockedByOther(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))
	ok, _, _ := store.CanEdit(context.Background(), "pg-1", "u-bob", true)
	if !ok {
		t.Fatal("admin should be able to edit despite a foreign lock")
	}
}

func TestCanEdit_FalseWhenDocStatusApproved(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(false, (*string)(nil), (*time.Time)(nil), "approved"))
	ok, reason, _ := store.CanEdit(context.Background(), "pg-1", "u-bob", false)
	if ok {
		t.Fatal("approved page should block edits")
	}
	if !strings.Contains(strings.ToLower(reason), "approved") {
		t.Fatalf("reason wrong: %q", reason)
	}
}

// ─── GetLockState ────────────────────────────────────

func TestGetLockState_ReturnsCurrent(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`SELECT.*FROM pages WHERE id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows(lockCols()).
			AddRow(true, ptrStr("u-alice"), &now, "draft"))
	state, err := store.GetLockState(context.Background(), "pg-1")
	if err != nil {
		t.Fatalf("GetLockState: %v", err)
	}
	if !state.Locked {
		t.Fatalf("unexpected: %+v", state)
	}
}
