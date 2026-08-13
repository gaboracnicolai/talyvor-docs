package search

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// EMPTYING A DOCUMENT DID NOT TAKE IT OUT OF SEMANTIC SEARCH — THE EARLY RETURN THAT SKIPS A PAID
// LENS CALL ALSO SKIPPED THE INVALIDATION, SO THE PAGE WENT ON ANSWERING FOR TEXT IT NO LONGER HELD.
//
// `IndexPage` opens `if strings.TrimSpace(text) == "" { return nil }`. That guard is about COST —
// there is nothing to embed, so do not pay Lens for it — and it was written as though "do not write
// a new vector" and "leave the old vector standing" were the same instruction. They are not:
// `page_embeddings` is an UPSERT keyed on page_id with no other writer and no delete path anywhere
// in the repository (whole-tree census: the only statements naming the table are this upsert and
// the SELECT in Search; the only removal is the 0004 FK's ON DELETE CASCADE, which needs the PAGE
// row to go).
//
// ⚠ THE TWO HALVES OF THE SAME SEARCH THEN DISAGREE ABOUT WHETHER THE DOCUMENT SAYS ANYTHING, and
// that is what makes this reachable rather than theoretical. `content_text` is emptied by the same
// save (`extractContentText` over an empty ProseMirror doc), so the FULL-TEXT half stops matching
// immediately — its index is a derived expression over the live column. The semantic half reads a
// SEPARATE, STORED copy that only this hook refreshes. So on `type=all`, the repo's default, a
// query for text the author deleted returns the page from one half and not the other, with the
// page's CURRENT title on the row.
//
// ⚠ WHAT IS AND IS NOT CLAIMED. The row carries no body text — a semantic-only row is page id,
// space id, title, space name and a cosine similarity — and `visibleTo` still runs, so this is not
// a disclosure of the deleted content and is not an authorization defect. It is a stale answer on
// the one axis a reader cannot check: the similarity is computed against a vector for text that is
// no longer in the document, so the ranking is derived from a state the product does not hold.
//
// ⚠ EMPTY IS THE ORDINARY WAY TO RETIRE A PAGE HERE, WHICH IS WHY THE GUARD IS ON THE EMPTY CASE
// rather than on a general staleness window. Docs has no archive and no `published` column
// (`model.Page` carries neither); `Delete` is destructive and takes the children with it via the
// reparent above it. Selecting the body and saving is what a person does when they mean "this no
// longer says anything", and it is a shipped PATCH: `content` is on `updatableFields`, and
// `contentChanged` fires on any `content` submitted as a string, INCLUDING an empty document.
//
// MEASURED THROUGH THE SHIPPED PATH ON REAL POSTGRES, before the fix — page.Store.Update with the
// real indexer wired, exactly as cmd/docs/main.go wires it, two pages saved and ONE emptied:
//
//	both saved with a body                     -> semantic search: 2 rows (both pages)
//	one saved as an EMPTY document (text = "")  -> semantic search: 2 rows, THE EMPTIED PAGE STILL
//	                                              AMONG THEM; page_embeddings still holds its row
//
// The assertion is on the SEARCH, not on the table, because the table is the mechanism and the
// answer is the product. [ROW-GONE] holds the mechanism separately so a repair that stops the row
// being served without removing the stored vector — leaving a paid-for artefact of deleted text on
// disk indefinitely — is not mistaken for this one.
func TestSemanticSearch_EmptyingAPageRetiresItsEmbedding_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	sp := seedSpace(t, d, ws, alice, "Finance", false)

	lens := newFakeLens(t)
	defer lens.Close()
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vecAt(0) + `}]}`))
	}
	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	// The store's index hook runs in a detached goroutine (store.go: "never block the editor's
	// save debounce on it"). Forwarding through a signalling wrapper waits on the REAL completion
	// rather than on a sleep, so the measurement below is of the product and not of a timer.
	idx := &signallingIndexer{real: sem, done: make(chan struct{}, 4)}
	store := page.NewStore(d.Pool).WithIndexer(idx)

	pageID := seedPage(t, d, ws, sp, alice, "Q3 numbers", "")
	// A SECOND, UNTOUCHED PAGE. It is not scenery: the repair is a DELETE, and a DELETE whose
	// WHERE clause is wrong or missing is the ordinary way this class of fix goes wrong. Nothing
	// else in this package indexes two pages and then empties one, so without this row an
	// over-broad retirement — one save clearing every embedding in the deployment — would be
	// indistinguishable from the correct one. [OTHER-PAGE] and [OTHER-ROW] hold it.
	otherID := seedPage(t, d, ws, sp, alice, "Q2 numbers", "")

	// SAVE ONE — both documents say something, and the shipped save indexes each.
	for _, p := range []struct{ id, body string }{
		{pageID, "the quarterly revenue figures for europe"},
		{otherID, "the quarterly revenue figures for asia"},
	} {
		if _, err := store.Update(ctx, p.id, map[string]any{
			"content":    proseDoc(p.body),
			"updated_by": alice,
		}); err != nil {
			t.Fatalf("first save %s: %v", p.id, err)
		}
		idx.wait(t, "first save")
	}

	// PRECONDITION, ASSERTED RATHER THAN ASSUMED: both pages ARE in the semantic index and ARE
	// returned. A green below must mean "emptying retired the one that was emptied", never "it was
	// never indexed" or "the fake Lens was not reached" — the vacuity this fixture can fail into.
	before, err := sem.Search(ctx, ws, "quarterly revenue", nil, 10, 0)
	if err != nil {
		t.Fatalf("[PRE-INDEXED] Search before: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("[PRE-INDEXED] both saved pages must be semantically searchable before this test "+
			"can say anything about emptying one: got %d rows %v, want %s and %s",
			len(before), ids(before), pageID, otherID)
	}

	// SAVE TWO — the author selects the body and deletes it. `content` is on updatableFields and
	// contentChanged fires on any content submitted as a string, so this is one shipped PATCH.
	if _, err := store.Update(ctx, pageID, map[string]any{
		"content":    emptyProseDoc,
		"updated_by": alice,
	}); err != nil {
		t.Fatalf("emptying save: %v", err)
	}
	idx.wait(t, "emptying save")

	// The premise of the whole test: the save really did empty the derived column. Without this a
	// failure below could be "extractContentText kept something", which is a different bug.
	var contentText string
	if err := d.Pool.QueryRow(ctx, `SELECT content_text FROM pages WHERE id = $1`, pageID).
		Scan(&contentText); err != nil {
		t.Fatalf("read content_text: %v", err)
	}
	if contentText != "" {
		t.Fatalf("[PRE-EMPTIED] the emptying save left content_text = %q — this fixture is not the "+
			"empty-document case it claims to be", contentText)
	}

	// ⚠ THE FOUR POST-CONDITIONS BELOW ARE `Errorf`, NOT `Fatalf`, AND THAT IS WHAT MAKES THEIR
	// TAGS MEAN ANYTHING. The two PRE-conditions above abort, correctly: a failed premise makes
	// everything after it meaningless. These four do not, because a `Fatalf` on the first of them
	// stops the rest from ever being EVALUATED — measured, not reasoned: with all four fatal, the
	// over-broad-DELETE control fired [OTHER-PAGE] and [OTHER-ROW] never ran, so [OTHER-ROW] was an
	// assertion no mutation could justify, sitting green because it was unreachable. Errorf lets
	// each control report its FULL catcher set, which is the only way a prediction is checkable.
	after, err := sem.Search(ctx, ws, "quarterly revenue", nil, 10, 0)
	if err != nil {
		t.Fatalf("Search after: %v", err)
	}

	// [SERVED] — THE PRODUCT ASSERTION. The emptied page must not answer a semantic query for text
	// it no longer contains.
	if contains(after, pageID) {
		t.Errorf("[SERVED] a page whose body was deleted is STILL returned by semantic search: "+
			"got %v — the stored vector still describes text the document does not hold, while the "+
			"full-text half (a derived index over the live column) has already stopped matching it",
			ids(after))
	}

	// [OTHER-PAGE] — THE BLAST RADIUS, ON THE SERVED SIDE. Emptying one document retires ONE
	// document. A retirement that reached further would empty the deployment's index one save at
	// a time, and every assertion above would still be green.
	if !contains(after, otherID) {
		t.Errorf("[OTHER-PAGE] emptying %s also removed the UNTOUCHED page %s from semantic "+
			"search: got %v — the retirement is not scoped to the page that was saved",
			pageID, otherID, ids(after))
	}

	// [ROW-GONE] — THE MECHANISM, HELD SEPARATELY ON PURPOSE. Serving is what a reader sees;
	// leaving the vector on disk keeps a paid-for artefact of deleted text indefinitely, and a
	// repair that only filtered the READ (a predicate in Search's SQL) would satisfy [SERVED] and
	// [OTHER-PAGE] exactly as the real fix does.
	if n := embeddingRows(t, d.Pool, pageID); n != 0 {
		t.Errorf("[ROW-GONE] the embedding row for an emptied page is still stored (%d rows) — "+
			"the vector of deleted text outlives the text", n)
	}

	// ⚠ THERE IS DELIBERATELY NO STORED-SIDE COMPANION TO [OTHER-PAGE], AND THE ASYMMETRY WITH
	// [ROW-GONE] IS THE REASON. `Search` reads `FROM page_embeddings pe JOIN pages p`, so a row
	// that is SERVED necessarily still has its embedding — the positive direction is implied and
	// an assertion on it can be red under no mutation [OTHER-PAGE] is green under. The negative
	// direction does NOT follow: absent from the results does not mean absent from the table,
	// which is exactly what the read-side near-miss control demonstrates, and that is why
	// [ROW-GONE] is a separate tag and its mirror image is not. An assertion the control run
	// cannot justify was deleted rather than retargeted.
}

func embeddingRows(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, pageID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*)::int FROM page_embeddings WHERE page_id = $1`, pageID).Scan(&n); err != nil {
		t.Fatalf("count embeddings for %s: %v", pageID, err)
	}
	return n
}

func contains(rs []SemanticResult, pageID string) bool {
	for _, r := range rs {
		if r.PageID == pageID {
			return true
		}
	}
	return false
}

// A PAGE THAT NEVER HAD CONTENT MUST NOT PAY LENS FOR THE PRIVILEGE OF BEING EMPTY.
//
// This is the must-stay-green companion to the guard above, and it is what keeps the repair from
// becoming "always call Lens". The cost guard in IndexPage is correct about the embed — an empty
// string has nothing to embed — and the whole finding is that it was ALSO deciding, silently, not
// to invalidate. [NO-EMBED] pins the half that was right.
func TestIndexPage_EmptyTextStillCostsNoLensCall_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	sp := seedSpace(t, d, ws, alice, "Finance", false)
	pageID := seedPage(t, d, ws, sp, alice, "Blank", "")

	lens := newFakeLens(t)
	defer lens.Close()
	var embeds int
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		embeds++
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vecAt(0) + `}]}`))
	}
	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	if err := sem.IndexPage(ctx, pageID, ws, "   \n\t "); err != nil {
		t.Fatalf("IndexPage(blank): %v", err)
	}
	if embeds != 0 {
		t.Fatalf("[NO-EMBED] indexing blank text made %d embedding request(s) to Lens — the "+
			"cost guard this finding is NOT about has been removed", embeds)
	}
}

// signallingIndexer forwards to the real indexer and reports completion. The store calls its hook
// from a detached goroutine; this waits on the actual call returning rather than on a duration.
type signallingIndexer struct {
	real *SemanticSearch
	done chan struct{}
}

func (s *signallingIndexer) IndexPage(ctx context.Context, pageID, workspaceID, text string) error {
	err := s.real.IndexPage(ctx, pageID, workspaceID, text)
	s.done <- struct{}{}
	return err
}

func (s *signallingIndexer) wait(t *testing.T, what string) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("the index hook never ran for the %s — the store's content hook did not fire, "+
			"so nothing below measures the indexer", what)
	}
}

// emptyProseDoc is what the editor submits for a document whose body has been deleted: a doc node
// with no children. extractContentText walks it to "".
const emptyProseDoc = `{"type":"doc","content":[]}`

func proseDoc(text string) string {
	return `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + text + `"}]}]}`
}

func ids(rs []SemanticResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.PageID)
	}
	return out
}
