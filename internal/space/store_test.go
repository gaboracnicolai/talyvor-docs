package space

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/model"
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

func spaceRow(id, slug string) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "workspace_id", "name", "slug", "description",
		"icon", "color", "private", "created_by",
		"created_at", "updated_at",
	}).AddRow(id, "ws-1", "Engineering", slug, "", "📄", "#6366f1",
		false, "creator", now, now)
}

// ─── Create ─────────────────────────────────────────────────

func TestCreate_AutoSlugsAndInserts(t *testing.T) {
	store, pool := newMockStore(t)
	// Caller leaves slug empty → store generates from name.
	pool.ExpectQuery(`INSERT INTO spaces`).
		WithArgs("ws-1", "Engineering", "engineering", "", "📄", "#6366f1",
			false, "creator").
		WillReturnRows(spaceRow("s-1", "engineering"))

	out, err := store.Create(context.Background(), model.Space{
		WorkspaceID: "ws-1",
		Name:        "Engineering",
		CreatedBy:   "creator",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out.Slug != "engineering" {
		t.Errorf("slug = %q", out.Slug)
	}
}

func TestCreate_RejectsEmptyName(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.Create(context.Background(), model.Space{
		WorkspaceID: "ws-1",
	})
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

// ─── GetBySlug ──────────────────────────────────────────────

func TestGetBySlug_Roundtrip(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT .* FROM spaces WHERE workspace_id = \$1 AND slug = \$2`).
		WithArgs("ws-1", "engineering").
		WillReturnRows(spaceRow("s-1", "engineering"))
	out, err := store.GetBySlug(context.Background(), "ws-1", "engineering")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if out.ID != "s-1" {
		t.Errorf("id = %q", out.ID)
	}
}

// ─── List ───────────────────────────────────────────────────

func TestList_ReturnsWorkspaceSpaces(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM spaces WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "workspace_id", "name", "slug", "description",
			"icon", "color", "private", "created_by",
			"created_at", "updated_at",
		}).
			AddRow("s-1", "ws-1", "Engineering", "engineering", "", "📄", "#6366f1", false, "creator", now, now).
			AddRow("s-2", "ws-1", "Design", "design", "", "🎨", "#ec4899", false, "creator", now, now))
	out, err := store.List(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("got %d, want 2", len(out))
	}
}

// ─── Update / Delete ────────────────────────────────────────

func TestUpdate_PatchesAllowlistedFields(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`UPDATE spaces SET`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(spaceRow("s-1", "engineering"))
	if _, err := store.Update(context.Background(), "s-1", map[string]any{
		"name": "Engineering renamed",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestDelete_RemovesSpace(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`DELETE FROM spaces`).
		WithArgs("s-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.Delete(context.Background(), "s-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
