package page_test

// A RENAME IS A SAVE, AND UNTIL THIS FILE IT OPENED NO RESTORE POINT.
//
// `page_versions` has exactly two content-bearing columns, title and content, and
// `RestoreVersion` writes BOTH of them back onto the live page. `Update` only appended a snapshot
// when `content` was submitted — so a save that changed only the title left the new name in no
// version row anywhere, and restoring the NEWEST version (the state the page is supposedly already
// in) renamed the live document back and destroyed the only copy of the rename.
//
// MEASURED THROUGH THE SHIPPED /v1 CHAIN ON REAL POSTGRES AT 78be685, in exactly the two PATCHes
// PageView.tsx sends: body save -> v1 ("Original", body:1); title-only save -> live title
// "Renamed by the user", STILL n=1 version; POST .../versions/1/restore -> live title back to
// "Original", content untouched. After the restore the string "Renamed by the user" appears in
// ZERO rows of the page or its history.
//
// TWO PRODUCERS OF A TITLE-ONLY UPDATE MAP, both non-test and both shipped — a census of the five
// non-test callers of the page store's Update (`cmd/docs/main.go:510` collab autosave,
// `handler.go:245` PATCH, `store.go:712` UpdateInWorkspaces, `store.go:994` RestoreVersion,
// `internal/mcp/server.go:856` update_page): the SPA's `flushTitle` sends `{title}` alone
// (PageView.tsx:150, a separate mutation from `onSaveBody`'s `{content, content_text}` at :142),
// and `toolUpdatePage` builds the map one PRESENT argument at a time, so an MCP client that names
// only `title` produces the same map. The autosave path is content-only and RestoreVersion sends
// both, so neither can reach it.
//
// WHY THE EXISTING GUARD WAS BLIND: `TestRestoreNewestVersion_LeavesTheLivePageUnchanged_RealPG`
// (versioning_title_pairing_test.go) asserts this exact user-facing property and is green,
// because every save in its fixture changes BOTH columns — which guarantees the newest version IS
// the live state. Its name is a claim about the product; its fixture is a claim about one shape of
// save. This file supplies the other shape.
//
// SEPARATE TEST FUNCTIONS, NOT SUBTESTS, so each is independently targetable by -run and a control
// verdict names exactly which assertion caught it. Controls:
// ~/talyvor-queue/w31-titleonly-controls-7e52.py

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// patchAs sends a PATCH body through the real /v1 chain as a gateway-verified member. asUser
// cannot be used here: it hard-codes a `{"title":"hacked-by-alice"}` body, and the body IS the
// variable under test in this file.
func patchAs(t *testing.T, chain http.Handler, path, email, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	req.Header.Set("X-User-Email", email)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	return rr
}

// [TITLE-SNAPSHOT] — a save that changes only the title appends a version row, and that row
// carries the title the save wrote together with the content the page currently has.
//
// Both columns are asserted, not just the count: a fix that appends a row but fills its content
// from a pre-update read would record a pair the page was never in — the defect `cf57f83` closed
// for the other column, re-opened on this path.
func TestTitleOnlySave_AppendsARestorePoint_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	// A body save first, so there is a history to append to and the content the rename must
	// snapshot is not the seed's.
	if _, err := store.Update(ctx, pID, map[string]any{
		"content": `{"body":"1"}`, "updated_by": alice,
	}); err != nil {
		t.Fatalf("seed body save: %v", err)
	}
	// The rename. No "content" key at all — this is PageView.tsx's flushTitle.
	if _, err := store.Update(ctx, pID, map[string]any{
		"title": "Renamed", "updated_by": alice,
	}); err != nil {
		t.Fatalf("title-only save: %v", err)
	}

	vers, err := store.GetVersions(ctx, pID)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vers) != 2 {
		t.Fatalf("[TITLE-SNAPSHOT] a title-only save opened no restore point: %d versions after "+
			"a body save and a rename, want 2 — the new name is in no version row, so restoring "+
			"the newest version will write the OLD name back over it", len(vers))
	}
	newest := vers[0] // GetVersions is ORDER BY version DESC
	if newest.Title != "Renamed" {
		t.Errorf("[TITLE-SNAPSHOT] newest version records title %q, want %q — the rename is not "+
			"in the history it created", newest.Title, "Renamed")
	}
	if newest.Content != `{"body":"1"}` {
		t.Errorf("[TITLE-SNAPSHOT] newest version records content %q, want %q — a rename's "+
			"snapshot must carry the content the page actually has, not a re-read from elsewhere",
			newest.Content, `{"body":"1"}`)
	}
}

// [NON-DESTRUCTIVE] — the user-facing consequence, through the shipped chain, asserted without
// reference to how a version row is built.
//
// The SPA labels this button "Restore this version (non-destructive — writes a new current
// version)" (VersionHistory.tsx:122). Restoring the newest version is a no-op by any reading of
// that label; after a title-only save it renamed the live page and left the discarded name in no
// row anywhere, so the undo the label promises did not exist.
func TestRestoreNewest_AfterATitleOnlySave_LeavesTheLivePage_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")
	sID := spaceOf(t, d, pID)

	chain := newV1Chain(t, d)
	base := "/v1/spaces/" + sID + "/pages/" + pID

	// PageView.tsx:142 — onSaveBody.
	if rr := patchAs(t, chain, base, "alice@corp.com",
		`{"content":"{\"body\":\"1\"}","content_text":"1"}`); rr.Code != http.StatusOK {
		t.Fatalf("body save = %d: %s", rr.Code, rr.Body.String())
	}
	// PageView.tsx:150 — flushTitle. Title only.
	if rr := patchAs(t, chain, base, "alice@corp.com",
		`{"title":"Renamed by the user"}`); rr.Code != http.StatusOK {
		t.Fatalf("title-only save = %d: %s", rr.Code, rr.Body.String())
	}

	ctx := context.Background()
	liveTitle := func() string {
		t.Helper()
		var title string
		if err := d.Pool.QueryRow(ctx, `SELECT title FROM pages WHERE id=$1`, pID).Scan(&title); err != nil {
			t.Fatalf("read live title: %v", err)
		}
		return title
	}
	before := liveTitle()
	if before != "Renamed by the user" {
		t.Fatalf("ANCHOR: the rename did not reach the live row (title=%q) — the rest of this "+
			"test would be asserting nothing", before)
	}

	// The newest version, read the way the SPA's history panel reads it.
	rrv := httptest.NewRequest(http.MethodGet, base+"/versions", nil)
	rrv.Header.Set("X-Gateway-Auth", testGatewaySecret)
	rrv.Header.Set("X-User-Email", "alice@corp.com")
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, rrv)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET versions = %d: %s", rec.Code, rec.Body.String())
	}
	var vs []model.PageVersion
	if err := json.Unmarshal(rec.Body.Bytes(), &vs); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if len(vs) == 0 {
		t.Fatalf("ANCHOR: no versions to restore")
	}
	newest := vs[0].Version // ORDER BY version DESC

	req := httptest.NewRequest(http.MethodPost, base+"/versions/"+strconv.Itoa(newest)+"/restore", nil)
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore v%d = %d: %s", newest, rec.Code, rec.Body.String())
	}

	if after := liveTitle(); after != before {
		t.Errorf("[NON-DESTRUCTIVE] restoring the NEWEST version (v%d) renamed the live page "+
			"%q -> %q. The button that did it is labelled non-destructive, and %q is now in no "+
			"page row and no version row — there is nothing left to restore it from",
			newest, before, after, before)
	}
}

// [ONE-SNAPSHOT] — a save that submits BOTH columns still takes exactly one snapshot.
//
// The floor for the obvious wrong fix: a second INSERT alongside content's, which would double
// every ordinary save's history and give RestoreVersion two identical rows to choose between.
func TestSaveOfBothColumns_TakesExactlyOneSnapshot_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	for i, s := range []struct{ title, content string }{
		{"A", `{"body":"1"}`},
		{"B", `{"body":"2"}`},
	} {
		if _, err := store.Update(ctx, pID, map[string]any{
			"title": s.title, "content": s.content, "updated_by": alice,
		}); err != nil {
			t.Fatalf("save %d: %v", i+1, err)
		}
	}
	vers, err := store.GetVersions(ctx, pID)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vers) != 2 {
		t.Errorf("[ONE-SNAPSHOT] two saves that each changed both columns produced %d versions, "+
			"want 2 — a save is one restore point, however many columns it touched", len(vers))
	}
}

// [NO-SNAPSHOT-FOR-UNVERSIONED-COLUMN] — a save of a column the version row does not carry opens
// no restore point.
//
// `page_versions` stores title and content and RestoreVersion writes back title and content; an
// icon is in neither, so a version whose only difference is an icon is a restore point to nothing.
// This is the floor for the over-broad fix — versioning on "any key present" rather than on the
// two columns that are actually snapshotted — and it is the assertion that makes the rule
// "submitted as a string, in the versioned set" rather than "submitted".
func TestIconOnlySave_TakesNoSnapshot_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	if _, err := store.Update(ctx, pID, map[string]any{
		"content": `{"body":"1"}`, "updated_by": alice,
	}); err != nil {
		t.Fatalf("seed body save: %v", err)
	}
	if _, err := store.Update(ctx, pID, map[string]any{
		"icon": "🚀", "updated_by": alice,
	}); err != nil {
		t.Fatalf("icon-only save: %v", err)
	}

	vers, err := store.GetVersions(ctx, pID)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vers) != 1 {
		t.Errorf("[NO-SNAPSHOT-FOR-UNVERSIONED-COLUMN] an icon-only save appended a version: %d "+
			"rows after one body save and one icon save, want 1 — an icon is in neither column a "+
			"version carries, so the row restores nothing", len(vers))
	}
}

type recordingLinker struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingLinker) SyncLinks(_ context.Context, _, _, content, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, content)
	return nil
}

func (r *recordingLinker) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// recordingIndexer records every IndexPage call. The store fires IndexPage in a goroutine, so
// there is no moment at which "it has not been called" is a fact — the recorder therefore also
// exposes waitFor, which blocks until a specific text has arrived.
type recordingIndexer struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingIndexer) IndexPage(_ context.Context, _, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, text)
	return nil
}

func (r *recordingIndexer) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recordingIndexer) waitFor(text string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, c := range r.seen() {
			if c == text {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// [NO-RESYNC] / [NO-REINDEX] — a rename does not re-run the two content hooks.
//
// Versioning a title-only save moves it into a block that also reconciled page_links and pushed
// the page through the semantic indexer. Both read a value a rename does not touch (SyncLinks
// takes out.Content, IndexPage takes out.ContentText), so re-running them on a rename buys
// nothing and IndexPage's is a paid Lens round-trip. Keeping the snapshot and the hooks on
// different conditions is the point of the split, and this is what holds them apart.
//
// ⚠ THE TWO OBSERVATIONS ARE NOT EQUALLY STRONG AND THE DIFFERENCE IS STRUCTURAL, SO IT IS
// WRITTEN DOWN RATHER THAN AVERAGED OVER. SyncLinks is called synchronously, so its absence after
// Update has returned is a FACT, and [NO-RESYNC] is a deterministic assertion. IndexPage is fired
// in a goroutine, so no instant exists at which "it was not called" is decidable: [NO-REINDEX]
// waits for a LATER content save's call to land and then allows a settle window before counting.
// A rename that indexed would have to have its goroutine — spawned first — still not scheduled
// after all of that. THE FIRST VERSION OF THIS ASSERTION WAS GREEN UNDER CONTROL E5 and only the
// control said so: it signalled through a BUFFERED channel, so the token the post-rename wait
// consumed was the RENAME'S OWN, and the wait it was built on returned before the call it was
// waiting for existed. The count then read two and passed.
func TestTitleOnlySave_DoesNotRunTheContentHooks_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	linker := &recordingLinker{}
	indexer := &recordingIndexer{}
	store := page.NewStore(d.Pool).WithLinker(linker).WithIndexer(indexer)
	ctx := context.Background()

	// REAL ProseMirror documents, unlike the `{"body":"N"}` placeholders the other tests in this
	// file use. IndexPage is handed content_text, and extractContentText returns "" for anything
	// without text nodes — with placeholders both saves index the empty string, the ANCHOR below
	// cannot tell them apart, and the wait it depends on is meaningless. Caught by that anchor.
	doc := func(s string) string {
		return `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + s + `"}]}]}`
	}

	if _, err := store.Update(ctx, pID, map[string]any{
		"content": doc("seed"), "updated_by": alice,
	}); err != nil {
		t.Fatalf("seed body save: %v", err)
	}
	if !indexer.waitFor("seed", 10*time.Second) {
		t.Fatalf("ANCHOR: the seed body save never reached the indexer (calls=%q) — this test "+
			"cannot observe an absence on a hook it has not seen fire", indexer.seen())
	}
	if got := linker.seen(); len(got) != 1 {
		t.Fatalf("ANCHOR: the seed body save called SyncLinks %d times, want 1", len(got))
	}

	// The rename.
	if _, err := store.Update(ctx, pID, map[string]any{
		"title": "Renamed", "updated_by": alice,
	}); err != nil {
		t.Fatalf("title-only save: %v", err)
	}
	if got := linker.seen(); len(got) != 1 {
		t.Errorf("[NO-RESYNC] a rename reconciled page_links: SyncLinks called %d times, want 1 "+
			"(the seed's). It is handed out.Content, which a title-only save does not change: "+
			"calls=%q", len(got), got)
	}

	// A content save AFTER the rename, with a text nothing else can produce, so the count below
	// is read only once a call spawned strictly later than the rename's has already landed.
	if _, err := store.Update(ctx, pID, map[string]any{
		"content": doc("final"), "updated_by": alice,
	}); err != nil {
		t.Fatalf("final body save: %v", err)
	}
	if !indexer.waitFor("final", 10*time.Second) {
		t.Fatalf("ANCHOR: the final body save never reached the indexer (calls=%q)", indexer.seen())
	}
	time.Sleep(250 * time.Millisecond) // settle window for a straggler spawned before "final"
	if got := indexer.seen(); len(got) != 2 {
		t.Errorf("[NO-REINDEX] a rename re-indexed the page: IndexPage called %d times, want 2 "+
			"(the two body saves). It is handed out.ContentText, which a title-only save does not "+
			"change, and the call is a paid Lens round-trip: calls=%q", len(got), got)
	}
}
