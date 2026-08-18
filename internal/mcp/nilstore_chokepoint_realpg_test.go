package mcp_test

// #141 PROVED get_page SURVIVES A NIL *space.Store. THREE OTHER DOORS INTO THE SAME STORE WERE
// NEVER GUARDED, AND ONE OF THEM IS THE AUTHORIZATION CHOKEPOINT.
//
// #141 made mcp.New convert a nil concrete pointer into a genuinely nil interface, then armed the
// ONE guard that had been dead: `if s.deps.spaces != nil` around the space-name enrichment inside
// get_page. typednil_deps_realpg_test.go pins exactly that path and is green. Nothing looked at
// the other derefs of the two stores New can now leave nil — a whole-file census finds them:
//
//	server.go:608  pageWorkspace   s.deps.pages.GetByID    ← SEC-4 chokepoint, no guard
//	server.go:620  spaceWorkspace  s.deps.spaces.GetByID   ← SEC-4 chokepoint, no guard
//	server.go:1189 toolGetSpaceTree s.deps.spaces.List     ← tool body, no guard
//
// toolWorkspace routes create_page and list_pages through spaceWorkspace, and get_page through
// spaceWorkspace TOO whenever it is called with space_id instead of page_id — so the very tool
// #141's test declares safe has a SECOND arm that reaches an unguarded deref. A green test on one
// arm read as a closed class.
//
// ⚠ WHY DENY AND NOT AN ERROR AT THE TWO RESOLVERS, AND IT IS THE FUNCTION'S OWN RULE RATHER THAN
// A CHOICE MADE HERE: pageWorkspace's doc comment already commits to it — "A missing/nonexistent
// page (or a lookup error) yields \"\" → deny — never a full-table leak" — and toolWorkspace
// repeats it ("Returns \"\" for an unmapped tool or an unresolvable object so the chokepoint
// denies (fail-closed)"). An unwired store is the most unresolvable an object gets. The cost is
// stated in-tree: a misconfigured server answers 'forbidden' rather than 'misconfigured' on these
// tools. That is the direction this chokepoint already chose, and the alternative reports server
// configuration state down an authorization path.
//
// toolGetSpaceTree is not a resolver but an ANSWER, so it takes the answer-surface idiom instead
// — the rpcError that get_stale_pages, ask_docs and get_page_analytics already return.
//
// RED/GREEN, stated per test:
//   - NilSpaceStore_ListPages          RED before (panic in the chokepoint), GREEN after.
//   - NilSpaceStore_GetSpaceTree       RED before (panic in the tool), GREEN after.
//   - NilPageStore_UpdatePage          RED before (panic in the chokepoint), GREEN after.
//   - NilSpaceStore_GetPageByIDStillServed  passes before AND after — #141's arm, the control
//     that stops this fix from regressing the path that already worked.
//   - WiredSpaceStore_GetSpaceTreeStillLists passes before AND after — the control that stops
//     "deny/error unconditionally" from satisfying the three tests above vacuously.

import (
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// noPanic turns a process-killing SIGSEGV into one readable failure. Without it the package's
// whole test binary dies and every other test in it reports nothing.
func noPanic(t *testing.T, tool string) func() {
	t.Helper()
	return func() {
		if rec := recover(); rec != nil {
			t.Fatalf("[PANIC] %s panicked on a server built without the store it dereferences: %v\n"+
				"mcp.New leaves the interface genuinely nil (#141), and this deref has no guard", tool, rec)
		}
	}
}

// RED BEFORE: list_pages is space-keyed, so toolWorkspace calls spaceWorkspace BEFORE any tool
// body runs — the panic is inside the authorization chokepoint itself.
func TestMCP_NilSpaceStore_ListPagesDoesNotPanic(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	d.Member(t, W, "owner@corp.com")

	srv := mcp.New(page.NewStore(d.Pool), nil, nil, nil, nil, "test").WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)
	defer noPanic(t, "list_pages")()

	rr := callTool(chain, "owner@corp.com", true, "list_pages", map[string]any{"space_id": "sp-anything"})
	// Reaching here at all is the assertion. An error (deny) is the correct answer; a RESULT
	// listing pages would mean the unresolvable space was authorized, which is a different and
	// worse failure — assert that too rather than accepting any non-panic.
	if result, errObj := rpcEnvelope(t, rr.Body.Bytes()); errObj == nil {
		t.Errorf("[CHOKEPOINT-DENIES] list_pages with NO space store returned a RESULT: %v\n"+
			"an unresolvable space must not authorize a listing", result)
	}
}

// RED BEFORE: get_space_tree is workspace-keyed, so it clears the chokepoint and panics in the
// tool body instead — a different site from the one above, reached a different way.
func TestMCP_NilSpaceStore_GetSpaceTreeDoesNotPanic(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	d.Member(t, W, "owner@corp.com")

	srv := mcp.New(page.NewStore(d.Pool), nil, nil, nil, nil, "test").WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)
	defer noPanic(t, "get_space_tree")()

	rr := callTool(chain, "owner@corp.com", true, "get_space_tree", map[string]any{"workspace_id": W})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		return // an error is the correct answer for an unwired store
	}
	// [TREE-NOT-EMPTY] an absent space store must not answer with an empty tree: that is the
	// positive claim "this workspace has no spaces", the same shape get_stale_pages' `[]` was.
	t.Errorf("[TREE-NOT-EMPTY] get_space_tree with NO space store returned a RESULT, not an "+
		"error: %s", contentText(t, result))
}

// RED BEFORE: the page-keyed half of the same chokepoint. deps.pages is nil-able by the same
// constructor and pageWorkspace dereferences it bare.
func TestMCP_NilPageStore_UpdatePageDoesNotPanic(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	d.Member(t, W, "owner@corp.com")

	srv := mcp.New(nil, space.NewStore(d.Pool), nil, nil, nil, "test").WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)
	defer noPanic(t, "update_page")()

	rr := callTool(chain, "owner@corp.com", true, "update_page",
		map[string]any{"page_id": "pg-anything", "title": "x"})
	if result, errObj := rpcEnvelope(t, rr.Body.Bytes()); errObj == nil {
		t.Errorf("[CHOKEPOINT-DENIES] update_page with NO page store returned a RESULT: %v\n"+
			"an unresolvable page must not authorize a write", result)
	}
}

// CONTROL (passes before AND after): #141's arm. get_page BY PAGE ID with a nil space store must
// still serve the page — this is the path typednil_deps_realpg_test.go already pins, repeated here
// so a regression shows up in the file that changed the surrounding code.
func TestMCP_NilSpaceStore_GetPageByIDStillServed(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	pgID := d.Page(t, W, owner, "Runbook")

	srv := mcp.New(page.NewStore(d.Pool), nil, nil, nil, nil, "test").WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "get_page", map[string]any{"page_id": pgID})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		t.Fatalf("[GETPAGE-STILL-SERVED] get_page by page_id errored with a nil space store: %v", errObj)
	}
	if text := contentText(t, result); !strings.Contains(text, "Runbook") {
		t.Errorf("[GETPAGE-STILL-SERVED] get_page did not carry the page title: %s", text)
	}
}

// CONTROL (passes before AND after): a WIRED space store must still produce the tree. Without
// this, "deny everything" and "error on every call" satisfy all three tests above and the tools
// are dead rather than guarded.
func TestMCP_WiredSpaceStore_GetSpaceTreeStillLists(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	seedSpaceP(t, d, W, owner, "Engineering", false)

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "get_space_tree", map[string]any{"workspace_id": W})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		t.Fatalf("[WIRED-TREE] get_space_tree errored with a REAL space store: %v", errObj)
	}
	// Asserting the space NAME, not merely "no error" — a guard that answers every call with an
	// empty tree would pass a bare error check.
	if text := contentText(t, result); !strings.Contains(text, "Engineering") {
		t.Errorf("[WIRED-TREE] wired space store produced a tree without the space: %s", text)
	}
}

// CONTROL (passes before AND after) — AND IT EXISTS BECAUSE A CONTROL RUN REFUTED A PREDICTION.
//
// M7 in the harness makes spaceWorkspace deny UNCONDITIONALLY. That was predicted to be caught by
// [WIRED-TREE] and was NOT CAUGHT: get_space_tree is WORKSPACE-keyed, so toolWorkspace answers it
// from the workspace_id argument and never calls spaceWorkspace at all. So the guard added to
// spaceWorkspace was asserted in ONE direction only — it stopped the panic, and nothing said it
// still ALLOWS a legitimate space-keyed call. A guard that denies everything would have shipped
// green.
//
// list_pages is space-keyed, so it is the tool that actually routes through that resolver.
func TestMCP_WiredSpaceStore_SpaceKeyedToolStillAuthorized(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	spaceID := seedSpaceP(t, d, W, owner, "Engineering", false)
	seedPageP(t, d, W, spaceID, owner, "Runbook", "body")

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "list_pages", map[string]any{"space_id": spaceID})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		t.Fatalf("[SPACEKEYED-ALLOWED] list_pages was DENIED for a space the caller owns: %v\n"+
			"spaceWorkspace resolved the space to no workspace, so the chokepoint refused", errObj)
	}
	// Assert the PAGE, not merely "no error" — a resolver that authorizes but returns nothing
	// would satisfy a bare error check.
	if text := contentText(t, result); !strings.Contains(text, "Runbook") {
		t.Errorf("[SPACEKEYED-ALLOWED] list_pages authorized but listed no page: %s", text)
	}
}
