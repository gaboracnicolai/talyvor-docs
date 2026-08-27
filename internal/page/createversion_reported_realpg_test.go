package page

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/testutil"
)

// THE FINDING, MEASURED ON REAL POSTGRES RATHER THAN READ.
//
// `page_versions` has exactly TWO writers in this repository and they are 100 lines apart
// in this file:
//
//	appendVersion  — the UPDATE path. #113 ("overlapping saves no longer lose restore
//	                 points") took its `_, _ =` away: it now captures the error, retries a
//	                 contested version number, and slog.Errors with an explicit `effect`.
//	                 Its own comment states the contract: "a snapshot that cannot be
//	                 written must not fail the save the user already committed. What
//	                 changed is that the failure is now REPORTED. `_, _ =` meant a lost
//	                 restore point and a successful-looking save were the same bytes in
//	                 the log."
//
//	Store.Create    — the CREATE path, and the ONLY writer of version 1. It still had
//	                 `_, _ = s.pool.Exec(...)`. The defect was found and fixed on one of
//	                 the two writers of one table and never swept to the other.
//
// MEASURED BEFORE THE FIX, by adding a CHECK constraint that makes the version INSERT
// fail exactly as a constraint, disk or permission problem would:
//
//	control (nothing in the way)  -> Create err=<nil>  page exists  versions=1
//	version INSERT forced to fail -> Create err=<nil>  page exists  versions=0
//	                                 GetVersions -> 0 rows, err=<nil>, and NOTHING logged
//
// ⚠ AND `Create`'s DOCSTRING CLAIMED THE OPPOSITE, IN THE STRONGEST FORM AVAILABLE:
// "The first version is appended to page_versions inside the same transaction so an
// aborted insert can't leave a page without an initial revision." There is no
// transaction. `pool.Begin` has never appeared in this file — the Phase 1 commit
// (13adc71) already used two separate pool calls with the error discarded — so the
// sentence has never been true, and the inline comment 100 lines below it says the
// reverse ("Failure here doesn't roll back the page itself").
//
// Version 1 is the base of every diff and the oldest restore point a page has.
//
// THE FIX KEEPS appendVersion's CONTRACT AND CHANGES ONLY WHAT #113 CHANGED: the create
// still succeeds, and the lost restore point is now REPORTED.

// forceVersionInsertToFail makes any future page_versions INSERT fail. NOT VALID so the
// constraint binds new rows only — an already-written control row must not block it.
func forceVersionInsertToFail(t *testing.T, d *testutil.DB) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`ALTER TABLE page_versions ADD CONSTRAINT test_force_fail CHECK (version < 0) NOT VALID`); err != nil {
		t.Fatalf("arrange: %v", err)
	}
}

func newSpace(t *testing.T, d *testutil.DB, ws string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by)
         VALUES ($1,'S',$2,'u-1') RETURNING id`, ws, "sp-"+ws).Scan(&id); err != nil {
		t.Fatalf("arrange space: %v", err)
	}
	return id
}

func countVersions(t *testing.T, d *testutil.DB, pageID string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM page_versions WHERE page_id=$1`, pageID).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

// createCapturingLog runs Create with slog captured, and returns the page and the log text.
func createCapturingLog(t *testing.T, s *Store, p model.Page) (*model.Page, string, error) {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out, err := s.Create(context.Background(), p)
	slog.SetDefault(prev)
	return out, buf.String(), err
}

// TestCreate_WhenTheV1SnapshotFails_TheLossIsReported_RealPG is the finding. A page
// created without its version 1 has no restore point and no diff base, and before this
// test that outcome was byte-identical in the log to a healthy create.
func TestCreate_WhenTheV1SnapshotFails_TheLossIsReported_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	spaceID := newSpace(t, d, ws)
	s := NewStore(d.Pool)
	forceVersionInsertToFail(t, d)

	out, logged, err := createCapturingLog(t, s, model.Page{
		SpaceID: spaceID, WorkspaceID: ws, Title: "no v1", CreatedBy: "u-1",
	})

	// The contract appendVersion states, and this fix deliberately preserves: a snapshot
	// that cannot be written must NOT fail the create.
	if err != nil {
		t.Fatalf("Create returned %v; a failed version snapshot must not fail the create "+
			"(appendVersion states that contract for the sibling path)", err)
	}
	if out == nil {
		t.Fatal("Create returned no page and no error")
	}
	if n := countVersions(t, d, out.ID); n != 0 {
		t.Fatalf("arrange failed: %d version rows, want 0 — the constraint did not bite, "+
			"so this test is measuring nothing", n)
	}

	if logged == "" {
		t.Fatalf("NOTHING WAS LOGGED. The page was created with no version 1 — no restore " +
			"point, no diff base — and a successful-looking create and a lost initial " +
			"revision produce the same bytes in the log. This is the `_, _ =` that #113 " +
			"removed from appendVersion, still present on the create path.")
	}
	for _, want := range []string{"page_id", out.ID, "effect"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the report does not contain %q, so an operator cannot act on it.\nlog: %s", want, logged)
		}
	}
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("the lost restore point was not reported at ERROR; appendVersion reports "+
			"the identical loss at ERROR.\nlog: %s", logged)
	}
}

// THE OTHER DIRECTION, so "always log an error" cannot pass the test above: a healthy
// create writes version 1 and reports nothing.
func TestCreate_HealthySnapshot_WritesV1AndReportsNothing_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	spaceID := newSpace(t, d, ws)
	s := NewStore(d.Pool)

	out, logged, err := createCapturingLog(t, s, model.Page{
		SpaceID: spaceID, WorkspaceID: ws, Title: "healthy", CreatedBy: "u-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if n := countVersions(t, d, out.ID); n != 1 {
		t.Fatalf("version rows after a healthy create = %d, want 1", n)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Errorf("a healthy create reported an ERROR; the report must mean something.\nlog: %s", logged)
	}

	// And v1 must still carry the page's own title, content and author — a fix that
	// reports the failure but writes the wrong snapshot is not a fix.
	var version int
	var title, content, createdBy, wsID string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT version, title, content, created_by, workspace_id
         FROM page_versions WHERE page_id=$1`, out.ID).
		Scan(&version, &title, &content, &createdBy, &wsID); err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if version != 1 {
		t.Errorf("first version = %d, want 1", version)
	}
	if title != out.Title || content != out.Content || createdBy != "u-1" || wsID != ws {
		t.Errorf("v1 = (%q, len(content)=%d, by %q, ws %q), want the page's own "+
			"(%q, len=%d, u-1, %s)", title, len(content), createdBy, wsID,
			out.Title, len(out.Content), ws)
	}
}

// TestCreate_DocstringDoesNotClaimATransactionItDoesNotHave is the claim half. The
// sentence "the first version is appended to page_versions inside the same transaction so
// an aborted insert can't leave a page without an initial revision" sat above this
// function from the Phase 1 commit and was NEVER true;
// TestCreate_WhenTheV1SnapshotFails_TheLossIsReported_RealPG is the executable proof. A
// comment is a claim about the file it sits in, and this one had no way to go stale loudly.
//
// ⚠ THE FIRST CUT OF THIS GUARD FORBADE THE WORDS "transaction", "atomic" and "roll back"
// ANYWHERE IN THE DOC COMMENT, AND IT FAILED ON THE CORRECTED DOCSTRING — which says, in
// as many words, that there IS no transaction. A vocabulary ban cannot tell an assertion
// from its denial, and the honest record of why this was wrong is exactly the text it
// banned. So the guard pins the FALSE CLAIM verbatim and REQUIRES the true one, which is
// both narrower and harder to satisfy by accident: a rewrite that quietly re-asserts
// atomicity has to delete "there is no transaction" to do it, and that is the half that
// then fails.
func TestCreate_DocstringDoesNotClaimATransactionItDoesNotHave(t *testing.T) {
	doc := normaliseComment(createDocComment(t))

	const falseClaim = "inside the same transaction so an aborted insert can't leave a page without an initial revision"
	if strings.Contains(doc, falseClaim) {
		t.Errorf("Create's docstring still makes the claim this item disproved:\n  %q\n"+
			"There is no transaction — pool.Begin has never appeared in this file — and the "+
			"version INSERT is a separate statement whose failure is tolerated by design.", falseClaim)
	}

	// And the true contract must be stated, so the claim cannot be removed by simply
	// saying nothing and letting the next reader assume atomicity.
	for _, required := range []string{"best-effort", "there is no transaction"} {
		if !strings.Contains(doc, required) {
			t.Errorf("Create's docstring no longer says %q. The snapshot is best-effort and the "+
				"create does not depend on it; a docstring that omits that invites the exact "+
				"assumption the old one asserted.\ndoc: %s", required, doc)
		}
	}
}

// createDocComment returns the doc comment immediately above Store.Create, read from
// store.go itself — a claim about a file is checked against that file, never a copy.
func createDocComment(t *testing.T) string {
	t.Helper()
	src := readStoreSource(t)
	i := strings.Index(src, "func (s *Store) Create(ctx context.Context, p model.Page)")
	if i < 0 {
		t.Fatal("Store.Create not found — this guard is aimed at nothing")
	}
	head := src[:i]
	start := strings.LastIndex(head, "\n\n")
	if start < 0 {
		t.Fatal("could not isolate Create's doc comment")
	}
	doc := head[start:]
	if !strings.Contains(doc, "// Create inserts a page") {
		t.Fatalf("isolated the wrong block; this guard must read Create's own doc comment.\ngot: %s", doc)
	}
	return doc
}

// normaliseComment strips comment markers and collapses whitespace so a claim is matched
// as a SENTENCE rather than as a particular line wrapping — rewrapping a paragraph must
// not silently disarm the guard.
func normaliseComment(doc string) string {
	out := strings.ReplaceAll(doc, "//", " ")
	return strings.ToLower(strings.Join(strings.Fields(out), " "))
}
