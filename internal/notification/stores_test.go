package notification

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --- recipients ---

func TestRecipients_EmailsByIDsMapsRows(t *testing.T) {
	pool := newMockPool(t)
	rs := newRecipientStore(pool)
	pool.ExpectQuery(`SELECT member_id, email, name FROM notification_recipients`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"member_id", "email", "name"}).
			AddRow("m1", "m1@x.z", "Alice").
			AddRow("m2", "m2@x.z", "Bob"))

	got, err := rs.EmailsByIDs(context.Background(), []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("EmailsByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d recipients, want 2 (m3 has no row → absent)", len(got))
	}
	if got["m1"].Email != "m1@x.z" || got["m2"].Name != "Bob" {
		t.Fatalf("unexpected mapping: %+v", got)
	}
	if _, ok := got["m3"]; ok {
		t.Error("m3 has no recipient row and must be absent (best-effort skip)")
	}
}

func TestRecipients_EmptyInputNoQuery(t *testing.T) {
	pool := newMockPool(t)
	rs := newRecipientStore(pool)
	got, err := rs.EmailsByIDs(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty input should return empty without a query; got %v err %v", got, err)
	}
}

// --- preferences (opt-out, default enabled) ---

func TestPreferences_DefaultsTrueWhenNoRow(t *testing.T) {
	pool := newMockPool(t)
	ps := newPreferenceStore(pool)
	pool.ExpectQuery(`SELECT email_enabled FROM notification_preferences`).
		WithArgs("m1", "page.mentioned").
		WillReturnRows(pgxmock.NewRows([]string{"email_enabled"}))
	ok, err := ps.IsEnabled(context.Background(), "m1", "page.mentioned")
	if err != nil || !ok {
		t.Fatalf("missing row should default enabled; got %v err %v", ok, err)
	}
}

func TestPreferences_EnabledMembersExcludesOptedOut(t *testing.T) {
	pool := newMockPool(t)
	ps := newPreferenceStore(pool)
	pool.ExpectQuery(`SELECT member_id FROM notification_preferences`).
		WithArgs("page.stale_digest", pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"member_id"}).AddRow("m2"))
	got, err := ps.EnabledMembers(context.Background(), "page.stale_digest", []string{"m1", "m2", "m3"})
	if err != nil {
		t.Fatalf("EnabledMembers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want m1 and m3", got)
	}
}
