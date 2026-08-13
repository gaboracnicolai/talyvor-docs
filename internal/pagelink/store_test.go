package pagelink

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
	// EVERY EXPECTATION IN THIS PACKAGE IS VERIFIED, NOT JUST THE ONES SOMEBODY REMEMBERED.
	//
	// pgxmock reports an expectation that was never called only if you ASK it. Nothing in this
	// package asked — 8 expectations, zero `ExpectationsWereMet` — and measurement
	// (scripts/w31-cross-package-write-controls.py, family D) showed what that cost HERE, in the
	// most misleading way of the six packages: BOTH writes vanishing DO redden something, and
	// NEITHER is caught by the test that names it.
	//
	//   · Delete Upsert's INSERT and TestSyncLinks_AddsAndRemovesEmbeds reds — because the
	//     surviving DELETE call is then matched against the INSERT expectation and mismatches on
	//     the query text. That is an ORDERING ACCIDENT of an ordered mock, not verification;
	//     TestUpsert_InsertsLink itself stays green.
	//   · Delete `Store.Delete`'s DELETE and the accident does not happen (the INSERT is
	//     consumed correctly and the DELETE expectation is simply left unfulfilled). What speaks
	//     instead is a test in ANOTHER PACKAGE — internal/trackintegration's
	//     TestSyncPageCosts_APageWithNoLinksIsWrittenAsZero, which reads the money back.
	//
	// So a green run here has never meant these writes happened.
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

func linkRow(id, pageID, issueID string) *pgxmock.Rows {
	now := time.Now().UTC()
	return pgxmock.NewRows([]string{
		"id", "page_id", "workspace_id", "issue_id", "link_type", "created_by", "created_at",
	}).AddRow(id, pageID, "ws-1", issueID, "embed", "u", now)
}

// ─── Upsert ───────────────────────────────────────────────

func TestUpsert_InsertsLink(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`INSERT INTO page_links`).
		WithArgs("p-1", "ws-1", "i-1", "embed", "u").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if err := store.Upsert(context.Background(), PageLink{
		PageID: "p-1", WorkspaceID: "ws-1", IssueID: "i-1",
		LinkType: "embed", CreatedBy: "u",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func TestUpsert_Idempotent(t *testing.T) {
	store, pool := newMockStore(t)
	// Second call with the same (page_id, issue_id, link_type) — the
	// store relies on the UNIQUE constraint + ON CONFLICT DO NOTHING.
	// The exec result reports 0 rows affected; the call must NOT
	// surface an error to the caller.
	pool.ExpectExec(`INSERT INTO page_links`).
		WithArgs("p-1", "ws-1", "i-1", "embed", "u").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	if err := store.Upsert(context.Background(), PageLink{
		PageID: "p-1", WorkspaceID: "ws-1", IssueID: "i-1",
		LinkType: "embed", CreatedBy: "u",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func TestUpsert_RejectsMissingFields(t *testing.T) {
	store, _ := newMockStore(t)
	if err := store.Upsert(context.Background(), PageLink{PageID: "", IssueID: "i"}); err == nil {
		t.Error("expected error for missing page_id")
	}
	if err := store.Upsert(context.Background(), PageLink{PageID: "p", IssueID: ""}); err == nil {
		t.Error("expected error for missing issue_id")
	}
}

// ─── Delete ───────────────────────────────────────────────

func TestDelete_RemovesLink(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectExec(`DELETE FROM page_links`).
		WithArgs("p-1", "i-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	if err := store.Delete(context.Background(), "p-1", "i-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// ─── ListByPage / ListByIssue ─────────────────────────────

func TestListByPage_ReturnsAllLinksForPage(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`FROM page_links WHERE page_id`).
		WithArgs("p-1").
		WillReturnRows(linkRow("l-1", "p-1", "i-1").
			AddRow("l-2", "p-1", "ws-1", "i-2", "embed", "u", time.Now()))
	out, err := store.ListByPage(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("ListByPage: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("got %d, want 2", len(out))
	}
}

func TestListByIssue_ReturnsAllLinksForIssue(t *testing.T) {
	store, pool := newMockStore(t)
	pool.ExpectQuery(`FROM page_links WHERE issue_id`).
		WithArgs("i-1").
		WillReturnRows(linkRow("l-1", "p-1", "i-1"))
	out, err := store.ListByIssue(context.Background(), "i-1")
	if err != nil {
		t.Fatalf("ListByIssue: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("got %d, want 1", len(out))
	}
}

// ─── ParseEmbeds (content scanner) ─────────────────────────

func TestParseEmbeds_FindsIssueIDsInProseMirrorJSON(t *testing.T) {
	doc := `{"type":"doc","content":[
        {"type":"paragraph","content":[
            {"type":"text","text":"see "},
            {"type":"issue_embed","attrs":{"issue_id":"i-1","identifier":"ENG-1"}},
            {"type":"text","text":" and "},
            {"type":"issue_embed","attrs":{"issue_id":"i-2","identifier":"ENG-2"}}
        ]},
        {"type":"callout","content":[{"type":"paragraph","content":[
            {"type":"issue_embed","attrs":{"issue_id":"i-3"}}
        ]}]}
    ]}`
	ids := ParseEmbeds(doc)
	want := map[string]bool{"i-1": true, "i-2": true, "i-3": true}
	if len(ids) != 3 {
		t.Fatalf("got %d ids, want 3 (%v)", len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id %q", id)
		}
	}
}

func TestParseEmbeds_MalformedJSONReturnsEmpty(t *testing.T) {
	if got := ParseEmbeds("not-json"); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// ─── SyncLinks (diff-based reconciliation) ────────────────

func TestSyncLinks_AddsAndRemovesEmbeds(t *testing.T) {
	store, pool := newMockStore(t)
	// Existing: i-1, i-2
	pool.ExpectQuery(`SELECT issue_id FROM page_links WHERE page_id`).
		WithArgs("p-1", "embed").
		WillReturnRows(pgxmock.NewRows([]string{"issue_id"}).
			AddRow("i-1").AddRow("i-2"))
	// New content has i-1 + i-3 — so add i-3, remove i-2.
	pool.ExpectExec(`INSERT INTO page_links`).
		WithArgs("p-1", "ws-1", "i-3", "embed", "u").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// THE THIRD ARGUMENT IS THE POINT, not plumbing: the removal pass deletes the `embed` row for
	// i-2 and nothing else typed on the same pair. pgxmock counts arguments, so dropping the
	// link_type scope again fails here loudly instead of passing as a silently wider DELETE.
	pool.ExpectExec(`DELETE FROM page_links`).
		WithArgs("p-1", "i-2", "embed").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	doc := `{"type":"doc","content":[{"type":"paragraph","content":[
        {"type":"issue_embed","attrs":{"issue_id":"i-1"}},
        {"type":"issue_embed","attrs":{"issue_id":"i-3"}}
    ]}]}`
	if err := store.SyncLinks(context.Background(), "p-1", "ws-1", doc, "u"); err != nil {
		t.Fatalf("SyncLinks: %v", err)
	}
}
