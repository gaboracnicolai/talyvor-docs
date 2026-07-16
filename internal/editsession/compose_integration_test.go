package editsession_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/editsession"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 2 — COMPOSE, DON'T REPLACE. store.Update's guard becomes
// approvalOK AND manualLockOK AND editSessionOK. This proves all three still gate a save
// through the composite, and that the edit-session ADDS single-writer without weakening the
// approval gate or the manual pagelock (their behavior is unchanged).
func TestCompose_GuardComposition_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	pageID := d.Page(t, ws, alice, "Composed doc")
	ctx := context.Background()

	lockStore := pagelock.NewStore(d.Pool)
	sessionStore := editsession.NewStore(d.Pool)
	// The exact wiring main.go uses.
	pages := page.NewStore(d.Pool).WithGuard(editsession.Compose(lockStore, sessionStore))

	save := func(member, content string) error {
		_, err := pages.Update(ctx, pageID, map[string]any{"content": content, "updated_by": member})
		return err
	}

	// Baseline: no session, no lock, not approved → anyone may save (composite = pagelock allow).
	if err := save(bob, `{"rev":"base"}`); err != nil {
		t.Fatalf("baseline save should succeed: %v", err)
	}

	// (1) EDIT-SESSION single-writer: Alice holds a live session → Bob's save is rejected
	// ("alice is editing"); Alice saves normally.
	if _, err := sessionStore.Acquire(ctx, pageID, []string{ws}, alice); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}
	err := save(bob, `{"rev":"bob-blocked"}`)
	if !errors.Is(err, page.ErrLocked) {
		t.Errorf("non-holder save = %v, want ErrLocked (single-writer)", err)
	}
	if err := save(alice, `{"rev":"alice-ok"}`); err != nil {
		t.Errorf("holder save should succeed: %v", err)
	}
	if err := sessionStore.Release(ctx, pageID, []string{ws}, alice); err != nil {
		t.Fatalf("release: %v", err)
	}

	// (2) MANUAL PAGELOCK unchanged: Alice manually locks → Bob blocked "Locked by alice",
	// even with NO edit-session active. The composite still honors the manual lock.
	if _, err := lockStore.Lock(ctx, pageID, alice); err != nil {
		t.Fatalf("manual lock: %v", err)
	}
	if err := save(bob, `{"rev":"bob-locked"}`); !errors.Is(err, page.ErrLocked) {
		t.Errorf("save under manual lock = %v, want ErrLocked (manual pagelock unchanged)", err)
	}
	if err := lockStore.Unlock(ctx, pageID, alice, false); err != nil {
		t.Fatalf("manual unlock: %v", err)
	}

	// (3) APPROVAL gate unchanged: an approved doc blocks ALL edits through the composite,
	// regardless of session/lock — exactly as pagelock.CanEdit did before.
	if _, err := d.Pool.Exec(ctx, `UPDATE pages SET doc_status = 'approved' WHERE id = $1`, pageID); err != nil {
		t.Fatalf("set approved: %v", err)
	}
	if err := save(alice, `{"rev":"approved-blocked"}`); !errors.Is(err, page.ErrLocked) {
		t.Errorf("save on approved doc = %v, want ErrLocked (approval gate unchanged)", err)
	}
}
