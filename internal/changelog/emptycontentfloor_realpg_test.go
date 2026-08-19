package changelog_test

// THE THIRD SITE OF THE CREATE-ONLY FLOOR, AND THE ONE NO HANDOVER HAD NAMED.
//
// `changelog_entries.content` is `TEXT NOT NULL DEFAULT '{}'` (migrations/0012_changelog.sql:19)
// and Store.CreateEntry normalises up to it (`if e.Content == "" { e.Content = "{}" }`,
// store.go:142-144) — the same three-line floor `page.Store.Create` and `block.Store.Create` carry.
// `Store.UpdateEntry` allow-lists `content` and copies the value into the SET list verbatim, so
// PATCH writes `''`.
//
// A CENSUS DECIDED THE SCOPE OF THIS MERGE RATHER THAN A SEARCH FOR MORE OF THE SAME. There are
// exactly four ProseMirror `content` columns declared `NOT NULL DEFAULT '{}'` in the twenty
// migrations — `pages`, `blocks`, `changelog_entries` and `library_templates`. Three have an
// UPDATE path that names `content`, and all three violated the floor. The fourth cannot: the only
// UPDATE statement against `library_templates` is `SET use_count = use_count + 1`
// (internal/templatelib/store.go:333), so its content is write-once and the floor holds by having
// no second writer. (`page_versions.content` and `page_comments.content` are `TEXT NOT NULL` with
// NO default — the first is derived from `pages.content` and inherits its floor through the
// snapshot, the second is a comment body and is plain text, not a document.)
//
// MEASURED THROUGH THE SHIPPED /v1 CHAIN (gatewayauth + authz + pageEnf) ON REAL POSTGRES.
//
// Controls: ~/talyvor-queue/scripts/w31-emptycontentfloor-controls-9a3d.py

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

const clFloorContent = `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"shipped the importer"}]}]}`

type clFloorFixture struct {
	d       *testutil.DB
	chain   http.Handler
	spaceID string
	pageID  string
	entryID string
}

func (f *clFloorFixture) send(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", clSecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rr := httptest.NewRecorder()
	f.chain.ServeHTTP(rr, req)
	return rr
}

func (f *clFloorFixture) patchEntry(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, http.MethodPatch,
		"/v1/spaces/"+f.spaceID+"/pages/"+f.pageID+"/changelog/entries/"+f.entryID, body)
}

func (f *clFloorFixture) content(t *testing.T) string {
	t.Helper()
	var c string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT content FROM changelog_entries WHERE id=$1`, f.entryID).Scan(&c); err != nil {
		t.Fatalf("read entry content: %v", err)
	}
	return c
}

// newCLFloorFixture creates one entry through the shipped POST route, carrying a real body.
func newCLFloorFixture(t *testing.T) *clFloorFixture {
	t.Helper()
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Release notes")
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("read the seeded page's space: %v", err)
	}
	f := &clFloorFixture{d: d, chain: clChain(d), spaceID: spaceID, pageID: pageID}

	seed, err := json.Marshal(map[string]any{
		"version": "1.2.3", "title": "1.2.3", "entry_type": "added", "content": clFloorContent,
	})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	rr := f.send(t, http.MethodPost,
		"/v1/spaces/"+spaceID+"/pages/"+pageID+"/changelog/entries", string(seed))
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("seeding the entry = %d %s, want 2xx — the fixture's own premise failed, so nothing "+
			"below is about the content floor", rr.Code, rr.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created entry: %v", err)
	}
	f.entryID = created.ID
	if got := f.content(t); got != clFloorContent {
		t.Fatalf("seeded content = %q, want %q — the fixture did not establish the state the cases "+
			"below measure a change from", got, clFloorContent)
	}
	return f
}

// [FLOOR] the finding.
func TestChangelogPatch_ExplicitEmptyContent_LandsAsTheEmptyDocument_RealPG(t *testing.T) {
	f := newCLFloorFixture(t)

	rr := f.patchEntry(t, `{"content":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH {\"content\":\"\"} = %d %s, want 200 — emptying an entry is a legal edit, "+
			"not a refusal", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != "{}" {
		t.Errorf("after PATCH {\"content\":\"\"} the row's content = %q, want %q — CreateEntry "+
			"normalises this exact value up to the column's `NOT NULL DEFAULT '{}'` fifty lines "+
			"above UpdateEntry", got, "{}")
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode PATCH response: %v", err)
	}
	if body.Content != "{}" {
		t.Errorf("PATCH response carried content=%q, want %q — the response must report the value "+
			"that was actually written", body.Content, "{}")
	}
}

// [CREATE-FLOOR] the half that was already correct and that NOTHING HELD, found by control C8.
// The block package's copy carries the full note. The [FLOOR] case above cannot cover this one: it
// PATCHes, so the Update floor answers first and CreateEntry is never asked.
func TestChangelogCreate_ExplicitEmptyContent_LandsAsTheEmptyDocument_RealPG(t *testing.T) {
	f := newCLFloorFixture(t)

	seed, err := json.Marshal(map[string]any{
		"version": "1.2.4", "title": "1.2.4", "entry_type": "added", "content": "",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rr := f.send(t, http.MethodPost,
		"/v1/spaces/"+f.spaceID+"/pages/"+f.pageID+"/changelog/entries", string(seed))
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("POST {\"content\":\"\"} = %d %s, want 2xx", rr.Code, rr.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created entry: %v", err)
	}
	var content string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT content FROM changelog_entries WHERE id=$1`, created.ID).Scan(&content); err != nil {
		t.Fatalf("read the created entry: %v", err)
	}
	if content != "{}" {
		t.Errorf("an entry created with content=\"\" holds %q, want %q — CreateEntry has floored "+
			"since 0012 shipped, and nothing asserted it", content, "{}")
	}
}

// [EDIT-STILL-WRITES] the must-stay-green companion.
func TestChangelogPatch_RealContent_SurvivesTheFloor_RealPG(t *testing.T) {
	f := newCLFloorFixture(t)

	const edited = `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"and the exporter"}]}]}`
	body, err := json.Marshal(map[string]any{"content": edited})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rr := f.patchEntry(t, string(body)); rr.Code != http.StatusOK {
		t.Fatalf("PATCH with a real body = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != edited {
		t.Errorf("after a real content PATCH the row = %q, want %q — the floor must normalise the "+
			"empty value and nothing else", got, edited)
	}
}

// [ABSENT-KEY-UNTOUCHED] the companion that keeps the floor off the absent key. UpdateEntry is
// allow-list-driven, so a key the client did not send is simply not in the SET list; a floor
// written as "content is always in the update map" would empty an entry on every title-only edit.
func TestChangelogPatch_AbsentContentKey_IsNotFloored_RealPG(t *testing.T) {
	f := newCLFloorFixture(t)

	if rr := f.patchEntry(t, `{"title":"1.2.3 (revised)"}`); rr.Code != http.StatusOK {
		t.Fatalf("title-only PATCH = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != clFloorContent {
		t.Errorf("after a title-only PATCH the row's content = %q, want %q unchanged — the floor "+
			"must fire on an EMPTY value, never on an ABSENT one", got, clFloorContent)
	}
}
