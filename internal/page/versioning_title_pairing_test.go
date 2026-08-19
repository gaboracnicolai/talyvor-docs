package page_test

// A version row is documented as "the state of a page" (model.PageVersion) and every row is
// documented as "a restore point" (store.Update). These guards assert that a row is a state the
// page WAS IN, rather than a pair assembled from two different moments.
//
// WHY NOTHING SAW THIS: the snapshot INSERT read its title and its content from different moments
// (a pre-update SELECT vs the UPDATE ... RETURNING row), so the two sources AGREE on every save
// that changes only ONE of the fields. A whole-repo census at cf57f83 found 6 test Update calls
// passing "content" and ZERO passing "title" — the disagreeing case had never been executed, so a
// lagged title was unobservable by construction rather than by bad luck.
//
// THREE SEPARATE TEST FUNCTIONS, NOT SUBTESTS, so each is independently targetable by -run and a
// control can name exactly which one must redden. Their coverage is different and measured:
// scripts/w31-version-title-controls.py has a mutation that only the content-only guard can see.

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// Each save's snapshot must carry the title AND the content that save wrote — not the title from
// the save before it. Three saves, each changing both fields, so a lag of exactly one shows up as
// a shift across the whole history rather than as one odd row.
func TestVersionSnapshot_PairsTitleAndContentFromTheSameSave_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	saves := []struct{ title, content string }{
		{"A", `{"body":"1"}`},
		{"B", `{"body":"2"}`},
		{"C", `{"body":"3"}`},
	}
	for i, s := range saves {
		if _, err := store.Update(ctx, pID, map[string]any{
			"title": s.title, "content": s.content, "updated_by": alice,
		}); err != nil {
			t.Fatalf("save %d (%s): %v", i+1, s.title, err)
		}
	}

	vers, err := store.GetVersions(ctx, pID, []string{ws})
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vers) != len(saves) {
		t.Fatalf("got %d versions, want %d", len(vers), len(saves))
	}
	for _, v := range vers { // GetVersions is ORDER BY version DESC
		if v.Version < 1 || v.Version > len(saves) {
			t.Fatalf("unexpected version number %d", v.Version)
		}
		want := saves[v.Version-1]
		if v.Title != want.title || v.Content != want.content {
			t.Errorf("MISPAIRED SNAPSHOT: version %d recorded (title=%q, content=%q); save %d wrote "+
				"(title=%q, content=%q) — the row is not a state this page was ever in",
				v.Version, v.Title, v.Content, v.Version, want.title, want.content)
		}
	}
}

// A save that changes ONLY the content must snapshot the title the page CURRENTLY has. This is
// the direction the two sources agreed on, so it is the direction a fix can silently break —
// reading the title from the request map instead of the saved row records an empty title here and
// is invisible to the guard above, which never makes a content-only save.
func TestVersionSnapshot_ContentOnlySave_KeepsTheCurrentTitle_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	if _, err := store.Update(ctx, pID, map[string]any{
		"title": "Renamed", "content": `{"body":"1"}`, "updated_by": alice,
	}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	// No "title" key at all — the page keeps the name it already has.
	if _, err := store.Update(ctx, pID, map[string]any{
		"content": `{"body":"2"}`, "updated_by": alice,
	}); err != nil {
		t.Fatalf("content-only save: %v", err)
	}

	vers, err := store.GetVersions(ctx, pID, []string{ws})
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vers) != 2 {
		t.Fatalf("got %d versions, want 2", len(vers))
	}
	newest := vers[0] // ORDER BY version DESC
	if newest.Title != "Renamed" || newest.Content != `{"body":"2"}` {
		t.Errorf("CONTENT-ONLY SAVE MISSNAPSHOTTED: version %d recorded (title=%q, content=%q); "+
			"want (title=%q, content=%q) — a save that does not rename must snapshot the current name",
			newest.Version, newest.Title, newest.Content, "Renamed", `{"body":"2"}`)
	}
}

// The user-facing consequence, asserted independently of how the row is stored: the newest
// version IS the state the page is already in, so restoring it must not change the live document.
// Through RestoreVersion → Update it silently renamed the live page to its previous title while
// leaving the content alone. The SPA advertises this button as "non-destructive"
// (frontend/src/components/VersionHistory.tsx).
func TestRestoreNewestVersion_LeavesTheLivePageUnchanged_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")

	store := page.NewStore(d.Pool)
	ctx := context.Background()

	if _, err := store.Update(ctx, pID, map[string]any{
		"title": "A", "content": `{"body":"1"}`, "updated_by": alice,
	}); err != nil {
		t.Fatalf("seed save: %v", err)
	}
	if _, err := store.Update(ctx, pID, map[string]any{
		"title": "B", "content": `{"body":"2"}`, "updated_by": alice,
	}); err != nil {
		t.Fatalf("save B: %v", err)
	}

	before, err := store.GetByID(ctx, pID)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	vers, err := store.GetVersions(ctx, pID, []string{ws})
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vers) == 0 {
		t.Fatalf("no versions to restore")
	}
	newest := vers[0].Version // ORDER BY version DESC

	if _, err := store.RestoreVersion(ctx, pID, newest, []string{ws}); err != nil {
		t.Fatalf("restore v%d: %v", newest, err)
	}

	after, err := store.GetByID(ctx, pID)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if after.Title != before.Title {
		t.Errorf("DESTRUCTIVE RESTORE: restoring the newest version (v%d) renamed the live page "+
			"%q -> %q; restoring the state the page is already in must not change it",
			newest, before.Title, after.Title)
	}
	if after.Content != before.Content {
		t.Errorf("DESTRUCTIVE RESTORE: restoring the newest version (v%d) changed live content "+
			"%q -> %q", newest, before.Content, after.Content)
	}
}
