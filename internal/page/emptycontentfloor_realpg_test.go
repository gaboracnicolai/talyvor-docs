package page_test

// THE `'{}'` FLOOR ON `pages.content` IS A CREATE-ONLY RULE, AND PATCH CAN WRITE THE VALUE THE
// FLOOR EXISTS TO PREVENT.
//
// `pages.content` is `TEXT NOT NULL DEFAULT '{}'` (migrations/0002_pages.sql:18) and Store.Create
// normalises an empty body up to that default (`if p.Content == "" { p.Content = emptyDoc }`,
// store.go:281) — so the column's own schema, its Create path and `emptyDoc`'s comment all state
// the same invariant: a page's body is always a JSON document, and the empty one is `{}`.
//
// Store.Update states nothing. `updates["content"]` goes into the SET list verbatim, so an
// explicit `{"content":""}` writes `''` — a value that is not JSON at all.
//
// MEASURED THROUGH THE SHIPPED /v1 CHAIN (gatewayauth + authz + pageEnf) ON REAL POSTGRES AT
// 71c96cf, not read off the source:
//
//	POST  /v1/spaces/{sp}/pages       {"title":"Fresh","content":""}  -> 201  content="{}"
//	PATCH /v1/spaces/{sp}/pages/{pg}  {"content":""}                  -> 200  content=""
//
// Two writers on one column, disagreeing about its floor, and the write path that can violate it
// is the one a client reaches every save.
//
// ⚠ IT IS DURABLE, WHICH IS WHY THIS COLUMN IS THE ONE THAT MATTERS AND NOT `blocks.content`.
// The same measurement showed the save appending `page_versions` row 2 with `content=""`. A
// snapshot is what `RestoreVersion` writes back and what `GET …/versions/{a}/diff/{b}` renders, so
// the non-JSON value is now a restore point rather than a transient row state. [VERSION-FLOOR]
// holds that half.
//
// ⚠⚠ NO READER IN THIS TREE BREAKS ON IT TODAY, AND THAT IS STATED HERE RATHER THAN LEFT FOR
// SOMEONE TO DISCOVER AS A REASON THIS TEST DID NOT NEED TO EXIST. Every consumer was written
// defensively against exactly this value, and each names it explicitly: `parseDoc`
// (frontend/src/hooks/useEditor.ts:143) returns undefined for `!json || json === "{}"`;
// `ParseEmbeds` (internal/pagelink/store.go:200) returns nil for the same pair, so emptying a page
// still clears its Track issue links; `prettyContent` (VersionHistory.tsx:48) and `extractText`
// (changelog/EntryCard.tsx:10) both try/catch to the raw string; the four export renderers
// `json.Unmarshal` and return on error; and `hasContent` (store.go:435) tests against `emptyDoc`
// AND `""`. THE DEFENSIVENESS IS THE EVIDENCE, not a reason to shrug: six readers each carry a
// special case for a value the schema says cannot exist. This test makes the schema's claim true
// so the seventh reader — the one that trusts the column and does not special-case it — is not
// the one that finds out.
//
// ⚠ WHAT THIS IS NOT: a refusal. `{"content":""}` means "empty this document" and that is a legal
// edit; `{}` IS the empty document. Normalising preserves the caller's intent exactly and makes
// Update agree with Create, where refusing would invent a 400 for a request the product supports.
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

// floorFixture seeds one page with a real body through the shipped PATCH route, so every case
// below starts from a document that has something to lose.
type floorFixture struct {
	d       *testutil.DB
	chain   http.Handler
	spaceID string
	pageID  string
}

const floorSeededContent = `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"the quarterly numbers"}]}]}`

// floorSeededJSON is floorSeededContent as a JSON string literal, for embedding in a request body.
const floorSeededJSON = `"{\"type\":\"doc\",\"content\":[{\"type\":\"paragraph\",\"content\":[{\"type\":\"text\",\"text\":\"the quarterly numbers\"}]}]}"`

func newFloorFixture(t *testing.T) *floorFixture {
	t.Helper()
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Quarterly")
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("read the seeded page's space: %v", err)
	}
	f := &floorFixture{d: d, chain: newV1Chain(t, d), spaceID: spaceID, pageID: pageID}
	rr := f.patch(t, `{"content":`+floorSeededJSON+`}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("seeding the page body = %d %s, want 200 — the fixture's own premise failed, so "+
			"nothing below is about the content floor", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != floorSeededContent {
		t.Fatalf("seeded content = %q, want %q — the fixture did not establish the state the cases "+
			"below measure a change from", got, floorSeededContent)
	}
	return f
}

func (f *floorFixture) patch(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return patchAs(t, f.chain, "/v1/spaces/"+f.spaceID+"/pages/"+f.pageID, "alice@corp.com", body)
}

// postAs is patchAs's POST twin — this package has no shared one, and [CREATE-FLOOR] needs the
// route Update's floor never reaches.
func postAs(t *testing.T, chain http.Handler, path, email, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	req.Header.Set("X-User-Email", email)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	return rr
}

// content reads the column straight out of Postgres. The response body is checked too, but the ROW
// is what the next reader gets and what the snapshot is taken from.
func (f *floorFixture) content(t *testing.T) string {
	t.Helper()
	var c string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT content FROM pages WHERE id=$1`, f.pageID).Scan(&c); err != nil {
		t.Fatalf("read page content: %v", err)
	}
	return c
}

// [FLOOR] the finding. An explicit empty body is a legal edit and must land as the canonical empty
// document, which is what Create would have written for the same value.
func TestPagePatch_ExplicitEmptyContent_LandsAsTheEmptyDocument_RealPG(t *testing.T) {
	f := newFloorFixture(t)

	rr := f.patch(t, `{"content":""}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH {\"content\":\"\"} = %d %s, want 200 — emptying a document is a legal edit, "+
			"not a refusal", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != "{}" {
		t.Errorf("after PATCH {\"content\":\"\"} the row's content = %q, want %q — `pages.content` is "+
			"NOT NULL DEFAULT '{}' and Store.Create normalises the same value up to that floor, so "+
			"Update writing %q puts a non-JSON body in a column whose schema, whose Create path and "+
			"whose `emptyDoc` constant all say it cannot hold one", got, "{}", got)
	}
	// The response must agree with the row. A 200 reporting content:"" tells the client the
	// column holds a value it does not hold — the response/row split #170 caught one field over.
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

// [VERSION-FLOOR] the durable half. The save appends a restore point, and a restore point holding
// a non-JSON body is one `RestoreVersion` writes back onto the live page and one the diff route
// renders.
func TestPagePatch_ExplicitEmptyContent_SnapshotsTheEmptyDocument_RealPG(t *testing.T) {
	f := newFloorFixture(t)

	if rr := f.patch(t, `{"content":""}`); rr.Code != http.StatusOK {
		t.Fatalf("PATCH {\"content\":\"\"} = %d %s, want 200", rr.Code, rr.Body.String())
	}

	var version int
	var snapshot string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT version, content FROM page_versions WHERE page_id=$1 ORDER BY version DESC LIMIT 1`,
		f.pageID).Scan(&version, &snapshot); err != nil {
		t.Fatalf("read the newest version row: %v", err)
	}
	// The precondition: the save must have appended a snapshot at all. Without this the assertion
	// below could be satisfied by a page whose history stopped at the seed.
	if version < 2 {
		t.Fatalf("[PRECONDITION] newest version = %d, want >= 2 — the emptying save appended no "+
			"restore point, so the assertion below is not about what this test claims", version)
	}
	if snapshot != "{}" {
		t.Errorf("page_versions v%d carries content = %q, want %q — RestoreVersion writes this "+
			"column back onto the live page and GET .../versions/{a}/diff/{b} renders it, so a "+
			"non-JSON body here is a restore point rather than a transient row state",
			version, snapshot, "{}")
	}
}

// [CREATE-FLOOR] the half that was already correct, added after control C8 found it UNHELD one
// package over — and this site's answer is DIFFERENT from that one, which is why the difference is
// written here rather than the block file's note being copied across.
//
// ⚠ C9 SAYS THIS COLUMN'S CREATE FLOOR WAS ALREADY COVERED, AND C8 SAYS THE BLOCK ONE WAS NOT.
// Deleting `if p.Content == "" { p.Content = emptyDoc }` reds `TestCreate_AutoSlugAndDepthAndVersion`
// (store_test.go:88) as well as this case. So the claim the block file makes — "cited as fact in
// four comments and asserted by nothing" — is TRUE THERE AND FALSE HERE, and stating it uniformly
// would have been a coverage claim the controls do not support.
//
// ⚠ WHAT THE PRE-EXISTING OWNER ACTUALLY HOLDS IS NARROWER THAN IT LOOKS, WHICH IS WHY THIS CASE
// IS STILL WORTH ITS LINES. That test is a pgxmock ARGUMENT-LIST assertion: it pins `"{}"` as the
// 6th WithArgs value of the INSERT, so it notices the floor only as a changed argument, and its own
// comment says the list exists to make an ARITY change visible. It is a statement-shape test on a
// mock. This case drives the shipped POST route on real Postgres and reads the column back, so
// what it holds is the behaviour rather than the call.
func TestPageCreate_ExplicitEmptyContent_LandsAsTheEmptyDocument_RealPG(t *testing.T) {
	f := newFloorFixture(t)

	rr := postAs(t, f.chain, "/v1/spaces/"+f.spaceID+"/pages", "alice@corp.com",
		`{"title":"Fresh","content":""}`)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("POST {\"content\":\"\"} = %d %s, want 2xx", rr.Code, rr.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode the created page: %v", err)
	}
	var content string
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT content FROM pages WHERE id=$1`, created.ID).Scan(&content); err != nil {
		t.Fatalf("read the created page: %v", err)
	}
	if content != "{}" {
		t.Errorf("a page created with content=\"\" holds %q, want %q — `emptyDoc`'s own comment "+
			"says Create defaults an absent body to it", content, "{}")
	}
}

// [EDIT-STILL-WRITES] the must-stay-green companion, and the one that stops the fix from being
// "always write {}". A real body must still land byte-for-byte.
//
// ⚠ IT IS NOT THE UNIQUE CATCHER, AND THE CONTROL SAYS SO RATHER THAN THE NAME. C2 (the floor
// firing on every value) reds ELEVEN tests in this package — this one plus the whole versioning
// suite, which notices because every snapshot it compares becomes `{}`. What this case uniquely
// pins is the byte-for-byte round-trip through the PATCH route; it is not the only thing standing
// between the fix and a route that destroys every document, and claiming otherwise would be the
// coverage-inflation this file's own controls exist to catch.
func TestPagePatch_RealContent_IsWrittenUnchanged_RealPG(t *testing.T) {
	f := newFloorFixture(t)

	const edited = `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"the revised numbers"}]}]}`
	const editedJSON = `"{\"type\":\"doc\",\"content\":[{\"type\":\"paragraph\",\"content\":[{\"type\":\"text\",\"text\":\"the revised numbers\"}]}]}"`
	if rr := f.patch(t, `{"content":`+editedJSON+`}`); rr.Code != http.StatusOK {
		t.Fatalf("PATCH with a real body = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != edited {
		t.Errorf("after a real content PATCH the row = %q, want %q — the floor must normalise the "+
			"empty value and nothing else", got, edited)
	}
}

// [ABSENT-KEY-UNTOUCHED] the other must-stay-green companion, and the one that stops the fix from
// reopening what `a5f5c6e`'s predecessor closed one package over: a PATCH that does not NAME
// content must leave the column exactly as it was. A normalisation that fires on the absent key
// would empty a document on every title-only save.
func TestPagePatch_AbsentContentKey_LeavesTheBodyIntact_RealPG(t *testing.T) {
	f := newFloorFixture(t)

	if rr := f.patch(t, `{"title":"Quarterly (renamed)"}`); rr.Code != http.StatusOK {
		t.Fatalf("title-only PATCH = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if got := f.content(t); got != floorSeededContent {
		t.Errorf("after a title-only PATCH the row's content = %q, want %q unchanged — a key the "+
			"client did not send must not be written, floor or no floor", got, floorSeededContent)
	}
}
