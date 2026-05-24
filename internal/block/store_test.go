package block

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

func blockRow(id, blockType string) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "page_id", "type", "content", "position", "parent_id",
		"created_at", "updated_at",
	}).AddRow(id, "pg-1", blockType, "{}", float64(0), (*string)(nil), now, now)
}

func TestCreate_InsertsBlock(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`INSERT INTO blocks`).
		WithArgs("pg-1", "paragraph", "{}", float64(1.5), (*string)(nil)).
		WillReturnRows(blockRow("b-1", "paragraph"))
	out, err := store.Create(context.Background(), model.Block{
		PageID: "pg-1", Type: "paragraph", Content: "{}", Position: 1.5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out.ID != "b-1" {
		t.Errorf("id = %q", out.ID)
	}
}

func TestListByPage_OrderedByPosition(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()
	pool.ExpectQuery(`FROM blocks WHERE page_id`).
		WithArgs("pg-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "page_id", "type", "content", "position", "parent_id",
			"created_at", "updated_at",
		}).
			AddRow("b-1", "pg-1", "heading", "{}", float64(1), (*string)(nil), now, now).
			AddRow("b-2", "pg-1", "paragraph", "{}", float64(2), (*string)(nil), now, now))
	out, err := store.ListByPage(context.Background(), "pg-1")
	if err != nil {
		t.Fatalf("ListByPage: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("got %d, want 2", len(out))
	}
}

func TestDelete_RemovesBlock(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`DELETE FROM blocks`).
		WithArgs("b-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.Delete(context.Background(), "b-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
