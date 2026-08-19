package mcp

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/model"
)

// get_space_tree's PAGE read had no error path, and the answer it produced when the read failed
// was the answer a genuinely empty space produces.
//
// MEASURED at 8e29fb8, through HandleRPC with a page store whose List returns an error:
//
//	failed read   →  {"result":{"content":[{"text":"[{\"space_id\":\"sp-1\",…,\"pages\":null}]"…
//	empty space   →  {"result":{"content":[{"text":"[{\"space_id\":\"sp-1\",…,\"pages\":null}]"…
//
// BYTE-IDENTICAL. The caller of this tool is an AI agent: "this space has no pages" is a claim it
// will act on and repeat, and it has no second channel to check the claim against. Every sibling
// read in server.go already refuses this exact trade — ask_docs stopped being `hits, _ :=` because
// a dead search made the model answer from nothing, get_stale_pages refuses to return `[]` for an
// absent engine because an empty stale report is the positive claim "nothing here needs
// attention", and get_space_tree's OWN header says "AN ABSENT STORE IS AN ERROR, NOT AN EMPTY
// TREE" twenty lines above the `pages, _ :=` this file exists to pin.
//
// ONE ASSERTION SET, AND THE MUST-STAY-GREEN COMPANION IS ONE THAT ALREADY EXISTED. Controls in
// scripts/w31-treeread-controls-8f5c.py, each run over the FULL suite against a real Postgres:
//
//	C1 restore `pages, _ :=` (the defect)    → TREE-UNREADABLE, and nothing else in 36 packages
//	C2 `continue` past the failing space     → TREE-UNREADABLE, and nothing else — a PARTIAL tree
//	                                           presented as complete is the same false claim one
//	                                           level up, which is why the fixture uses TWO spaces
//	C4 C1 with THIS FILE DELETED             → the FULL suite is GREEN. The measured blindness:
//	                                           nothing else in the repository can see this defect
//
// ⚠ AND THE OVER-CORRECTION IS ALREADY GUARDED, WHICH I PREDICTED WRONG AND THE CONTROL SAID SO.
// This file first carried a companion asserting that a genuinely empty space still ANSWERS (so
// "report the failure" could not be satisfied by failing on everything). Both of its mutations —
// C3 an empty page list treated as an error, C5 a space with no pages dropped from the tree — are
// caught by TestMCP_WiredSpaceStore_GetSpaceTreeStillLists ([WIRED-TREE],
// nilstore_chokepoint_realpg_test.go), which seeds a page-less space against REAL Postgres and
// asserts the space name is in the tree. My prediction for C3 listed only my own tag. The
// companion was DELETED rather than retargeted: no mutation catches it that [WIRED-TREE] does not
// catch first, and it was the weaker of the two (fakes, not a real store).
func TestGetSpaceTree_UnreadableSpaceIsAnError_NotAnEmptyOrPartialTree(t *testing.T) {
	// TWO spaces, and only the SECOND is unreadable. A fixture with one space cannot tell a tool
	// that reports the failure from a tool that quietly drops the space and answers with what it
	// did manage to read.
	spaces := &fakeSpaces{list: []model.Space{
		{ID: "sp-readable", Name: "Engineering"},
		{ID: "sp-broken", Name: "Finance"},
	}}
	pages := &fakePages{
		list:       []model.Page{{ID: "pg-1", Title: "Index", SpaceID: "sp-readable"}},
		listErr:    errors.New("read pages: connection refused"),
		listErrFor: map[string]bool{"sp-broken": true},
	}
	srv := newTestServer(t, pages, spaces, &fakeAnalytics{}, &fakeAI{}, nil)

	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, rpc("tools/call", map[string]any{
		"name":      "get_space_tree",
		"arguments": map[string]any{"workspace_id": testWS},
	}, 1))
	resp := decodeResp(t, rr)

	if resp["error"] == nil {
		body, _ := json.Marshal(resp)
		t.Fatalf("[TREE-UNREADABLE] a space whose page read FAILED was answered with a result, "+
			"not an error — an agent cannot tell this from an empty or complete tree: %s", body)
	}
	errObj, _ := resp["error"].(map[string]any)
	if code, _ := errObj["code"].(float64); int(code) != errInternal {
		t.Errorf("[TREE-UNREADABLE-CODE] want errInternal (%d), got %v — a read failure is not a "+
			"client mistake and must not be reported as one", errInternal, errObj["code"])
	}
	if resp["result"] != nil {
		body, _ := json.Marshal(resp)
		t.Errorf("[TREE-PARTIAL] a failure carried a result alongside it: %s", body)
	}
}
