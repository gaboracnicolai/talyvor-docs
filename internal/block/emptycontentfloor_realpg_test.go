package block_test

// THE SECOND HALF OF patchsemantics_realpg_test.go's CLOSING NOTE, MEASURED AND CLOSED.
//
// That file fixed the IMPLICIT path — an absent `content` key arriving as Go's zero value and
// being written — and recorded what it deliberately left open:
//
//	"an EXPLICIT {"content":""} still writes an empty string, where Create would have normalised
//	 it to "{}". That asymmetry is a second finding and wants its own merge; it is now the only
//	 way to reach the empty value, and it takes a client that means it."
//
// This is that merge. `blocks.content` is `TEXT NOT NULL DEFAULT '{}'` (migrations/0002_pages.sql:78)
// and Store.Create normalises up to it (`if b.Content == "" { b.Content = "{}" }`, store.go:59);
// Store.Update's `COALESCE($1::text, content)` distinguishes ABSENT from EMPTY correctly — which is
// exactly what the earlier merge bought — and then writes the empty one through.
//
// MEASURED THROUGH THE SHIPPED /v1 CHAIN (gatewayauth + authz + blockEnf) ON REAL POSTGRES AT
// 71c96cf: POST {"content":""} -> 201 content="{}"; PATCH {"content":""} -> 200 content="".
// Two writers, one column, one floor, applied by one of them.
//
// ⚠ THIS COLUMN IS THE LOWER-STAKES SITE OF THE CLASS AND IS FIXED HERE ANYWAY, because a floor
// that holds in two packages out of three is not a floor. The stakes live on `pages.content` —
// the canonical ProseMirror blob this package's own header defers to ("the page render path still
// reads pages.content ... until the editor ships"), which is versioned, link-synced and indexed.
// internal/page/emptycontentfloor_realpg_test.go carries that half and the reader census.
//
// Controls: ~/talyvor-queue/scripts/w31-emptycontentfloor-controls-9a3d.py

import (
	"encoding/json"
	"net/http"
	"testing"
)

// [FLOOR] the finding. An explicit empty body must land as the canonical empty document — the
// value Create writes for the same input.
func TestBlockPatch_ExplicitEmptyContent_LandsAsTheEmptyDocument_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, seededContent, 3)

	rr := f.do(t, asBlockUser(http.MethodPatch, "/v1/blocks/"+id, "alice@corp.com", `{"content":""}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH {\"content\":\"\"} = %d %s, want 200 — emptying a block is a legal edit, not "+
			"a refusal", rr.Code, rr.Body.String())
	}

	content, position := f.row(t, id)
	if content != "{}" {
		t.Errorf("after PATCH {\"content\":\"\"} the row's content = %q, want %q — Store.Create "+
			"normalises this exact value up to the column's `NOT NULL DEFAULT '{}'` thirty lines "+
			"above Update, and this file's sibling recorded the asymmetry rather than fixing it",
			content, "{}")
	}
	if position != 3 {
		t.Errorf("position = %v, want 3 unchanged — the partial-update rule must survive the floor", position)
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
//
// ⚠⚠ THIS CASE EXISTS BECAUSE A CONTROL FALSIFIED ITS OWN PREDICTION. C8 deletes Create's floor —
// three lines that have been in store.go since the file was written — and predicted a red. The
// whole package went GREEN. The floor is cited as established fact in FOUR comments across this
// repository, including the sentence in patchsemantics_realpg_test.go that justified the SHAPE of
// this very fix ("block.Store.Create, thirty lines above the broken Update in the same file,
// refuses to store an empty content at all"), and no test in the tree asserted it. It could have
// been deleted in any refactor and every gate would have stayed green.
//
// The new [FLOOR] case above does not cover it either, and that is worth saying plainly rather
// than leaving as an inference: it PATCHes, so the Update floor answers first and Create is never
// asked. A guard for the write path is not a guard for the other write path.
//
// ⚠ THE GAP IS THIS PACKAGE'S AND changelog's, NOT page's — C9 reds `TestCreate_AutoSlugAndDepth-
// AndVersion` when the page floor goes, so that column had an owner (a pgxmock argument-list
// assertion; see the note on the page file's copy). Three sites, two of them uncovered, and only
// running the control per site told them apart.
func TestBlockCreate_ExplicitEmptyContent_LandsAsTheEmptyDocument_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, "", 1)

	if content, _ := f.row(t, id); content != "{}" {
		t.Errorf("a block created with content=\"\" holds %q, want %q — the column is "+
			"`NOT NULL DEFAULT '{}'` and Create has normalised up to it since this file was "+
			"written; four comments in this repository rest on that being true", content, "{}")
	}
}

// [EDIT-STILL-WRITES] the must-stay-green companion. Without it the floor could be satisfied by a
// route that writes `{}` for every body.
//
// ⚠ IT IS NOT THE UNIQUE CATCHER OF THE MUTATION IT WAS WRITTEN FOR, and the control says so:
// C2's page-side equivalent reds ELEVEN tests, most of them the pre-existing versioning suite. The
// honest claim is narrower than "this is what stops the fix destroying every document" — it is the
// only case that pins the byte-for-byte round-trip through THIS route, and the versioning suite
// covers the same mutation from the other end.
func TestBlockPatch_RealContent_SurvivesTheFloor_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, seededContent, 3)

	const edited = `{"type":"doc","text":"still a document"}`
	body, err := json.Marshal(map[string]any{"content": edited})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rr := f.do(t, asBlockUser(http.MethodPatch, "/v1/blocks/"+id, "alice@corp.com", string(body))); rr.Code != http.StatusOK {
		t.Fatalf("PATCH with a real body = %d %s, want 200", rr.Code, rr.Body.String())
	}
	if content, _ := f.row(t, id); content != edited {
		t.Errorf("after a real content PATCH the row = %q, want %q — the floor must normalise the "+
			"empty value and nothing else", content, edited)
	}
}

// [ABSENT-KEY-UNTOUCHED] the companion that pins what the PREVIOUS merge bought, re-asserted
// THROUGH the floor. A normalisation applied to the nil pointer rather than to the empty string
// would re-destroy the document on every drag-and-drop reorder — the exact defect
// patchsemantics_realpg_test.go exists for, reintroduced by its own follow-up.
func TestBlockPatch_AbsentContentKey_IsNotFloored_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, seededContent, 3)

	if rr := f.do(t, asBlockUser(http.MethodPatch, "/v1/blocks/"+id, "alice@corp.com", `{"position":9}`)); rr.Code != http.StatusOK {
		t.Fatalf("position-only PATCH = %d %s, want 200", rr.Code, rr.Body.String())
	}
	content, position := f.row(t, id)
	if content != seededContent {
		t.Errorf("after PATCH {\"position\":9} the row's content = %q, want %q unchanged — the floor "+
			"must fire on an EMPTY value, never on an ABSENT one", content, seededContent)
	}
	if position != 9 {
		t.Errorf("position = %v, want 9 — the field that WAS sent must still be written", position)
	}
}
