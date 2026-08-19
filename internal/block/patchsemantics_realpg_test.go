package block_test

// PATCH /v1/blocks/{blockID} WAS A WHOLESALE REPLACE, AND EVERY ABSENT FIELD'S ZERO VALUE IS
// DESTRUCTIVE.
//
// handler.Update decoded the body into a VALUE struct — `struct{ Content string; Position float64 }`
// — and handed both to the store, which wrote `SET content = $1, position = $2` unconditionally. A
// JSON key the client did not send therefore arrived as Go's zero value and was WRITTEN:
//
//	PATCH {"position":9}        → content becomes ""  — the whole ProseMirror node destroyed, and
//	                              '' is not even valid JSON in a column whose schema default is '{}'
//	                              (migrations/0002_pages.sql).
//	PATCH {"content":"…"}       → position becomes 0  — the block silently jumps to the top of the
//	                              page (blocks are read `ORDER BY position`, idx_blocks_page).
//
// MEASURED before the fix, through the SHIPPED chain (gatewayauth + authz + blockEnf) on real
// Postgres, not read off the source. Seeded `content={"type":"doc","text":"the quarterly numbers"}`,
// `position=3`, then the request a drag-and-drop reorder sends:
//
//	PATCH {"position":9}  -> 200  {"content":"","position":9}   row: content="" position=9
//	PATCH {"content":…}   -> 200  {"content":"{\"t\":2}","position":0}   (seeded position was 7)
//
// ⚠ THE VERB IS THE CLAIM. PATCH is a PARTIAL update by definition (RFC 5789) and the route is
// registered with chi's `Patch`. A client sending only the field it changed — the ordinary,
// correct thing to do — silently lost the other one, with a 200 and a response body that looked
// like a successful edit.
//
// ⚠ THE REPO ALREADY KNEW THE RIGHT SHAPE, ONE PACKAGE OVER. page.Handler.Update decodes into
// `map[string]any` and passes an allowlist to the store, so an absent key is simply not in the
// UPDATE. And block.Store.Create, thirty lines above the broken Update in the same file, refuses
// to store an empty content at all (`if b.Content == "" { b.Content = "{}" }`) — the asymmetry was
// inside one file.
//
// ⚠ WHY NOTHING CAUGHT IT: this HTTP surface has NO caller and had NO coverage. A zero-coverage
// census over the whole repo at 23c4884 (69 production functions at 0.0%) named block.Handler
// List/Create/Delete; Update was reached only by failclosed_gate_test.go, which calls the STORE
// directly and always passes both values, so no test in the repository had ever sent a partial
// body through the decoder that produced the zero value.
//
// The fix is the COALESCE shape, in ONE statement rather than a read-modify-write: an absent field
// arrives as a nil pointer, becomes SQL NULL, and `COALESCE($n, column)` keeps what is there. A
// separate SELECT-then-UPDATE would leave a window in which a concurrent edit is lost — the same
// reasoning Store.Create already records for folding its parent check into the INSERT.
//
// ⚠ WHAT IS DELIBERATELY NOT CHANGED HERE, so it is not mistaken for an oversight: an EXPLICIT
// `{"content":""}` still writes an empty string, where Create would have normalised it to "{}".
// That asymmetry is a second finding and wants its own merge; it is now the only way to reach the
// empty value, and it takes a client that means it.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/block"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const patchGatewaySecret = "block-patch-test-gateway-secret-0123"

// newBlockChain mirrors cmd/docs/main.go's wiring for the block routes: gatewayauth + authz on
// the /v1 group, pageEnf for /pages/{pageID}/blocks, and blockEnf — which resolves the owning
// page FROM the block id — for /blocks/{blockID}. Driving the real chain is what makes the
// measurements above statements about the product rather than about a store method.
func newBlockChain(t *testing.T, d *testutil.DB) http.Handler {
	t.Helper()
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	blockPageLooker := func(ctx context.Context, blockID string) (string, permission.PageMeta, error) {
		var pageID string
		if err := d.Pool.QueryRow(ctx, `SELECT page_id FROM blocks WHERE id=$1`, blockID).Scan(&pageID); err != nil {
			return "", permission.PageMeta{}, err
		}
		md, err := pageLooker(ctx, pageID)
		if err != nil {
			return "", permission.PageMeta{}, err
		}
		return pageID, md, nil
	}
	h := block.NewHandler(block.NewStore(d.Pool)).WithAccess(
		permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore)),
		permission.NewEnforcer(permStore, permission.PageResolverFromBlock("blockID", blockPageLooker, permStore)),
	)
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(patchGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

// asBlockUser forges a gateway-verified request. No X-Member-Id / X-Talyvor-Workspace: identity
// comes only from the verified email, exactly as SEC-4 requires.
func asBlockUser(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", patchGatewaySecret)
	r.Header.Set("X-User-Email", email)
	return r
}

type patchFixture struct {
	chain http.Handler
	d     *testutil.DB
	page  string
}

func newPatchFixture(t *testing.T) *patchFixture {
	t.Helper()
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	return &patchFixture{chain: newBlockChain(t, d), d: d, page: d.Page(t, ws, alice, "quarterly review")}
}

func (f *patchFixture) do(t *testing.T, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	f.chain.ServeHTTP(rr, r)
	return rr
}

// seedBlock creates a block through the SHIPPED POST route and returns its id.
func (f *patchFixture) seedBlock(t *testing.T, content string, position float64) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"type": "paragraph", "content": content, "position": position})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	rr := f.do(t, asBlockUser(http.MethodPost, "/v1/pages/"+f.page+"/blocks", "alice@corp.com", string(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed block: %d %s — the fixture's own premise failed, nothing below is about the "+
			"partial-update rule", rr.Code, rr.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode seeded block: %v", err)
	}
	return out.ID
}

// row reads the block straight out of Postgres. The response body is checked too, but the ROW is
// what a later reader gets, and the two disagreeing is itself worth catching.
func (f *patchFixture) row(t *testing.T, id string) (string, float64) {
	t.Helper()
	var content string
	var position float64
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT content, position FROM blocks WHERE id=$1`, id).Scan(&content, &position); err != nil {
		t.Fatalf("read block row: %v", err)
	}
	return content, position
}

const seededContent = `{"type":"doc","text":"the quarterly numbers"}`

// [REORDER-KEEPS-CONTENT] the finding, in the direction that destroys a document. A drag-and-drop
// reorder sends the new position and nothing else.
func TestBlockPatch_PositionOnly_LeavesContentIntact_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, seededContent, 3)

	rr := f.do(t, asBlockUser(http.MethodPatch, "/v1/blocks/"+id, "alice@corp.com", `{"position":9}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH position-only = %d %s, want 200", rr.Code, rr.Body.String())
	}

	content, position := f.row(t, id)
	if content != seededContent {
		t.Errorf("after PATCH {\"position\":9} the row's content = %q, want %q unchanged — an absent "+
			"JSON key was written as Go's zero value and destroyed the block's document",
			content, seededContent)
	}
	if position != 9 {
		t.Errorf("after PATCH {\"position\":9} the row's position = %v, want 9 — the field that WAS "+
			"sent must still be written", position)
	}
	// The response body must agree with the row: a 200 carrying content:"" told the client the
	// edit had succeeded while it had in fact emptied the block.
	var body struct {
		Content  string  `json:"content"`
		Position float64 `json:"position"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode PATCH response: %v", err)
	}
	if body.Content != seededContent || body.Position != 9 {
		t.Errorf("PATCH response = {content:%q position:%v}, want {content:%q position:9} — the "+
			"response must not report an edit the row did not receive", body.Content, body.Position, seededContent)
	}
}

// [EDIT-KEEPS-POSITION] the other direction. A content edit must not silently move the block to
// the top of the page — blocks are listed `ORDER BY position`.
func TestBlockPatch_ContentOnly_LeavesPositionIntact_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, seededContent, 7)

	const edited = `{"type":"doc","text":"the revised numbers"}`
	body, err := json.Marshal(map[string]any{"content": edited})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rr := f.do(t, asBlockUser(http.MethodPatch, "/v1/blocks/"+id, "alice@corp.com", string(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH content-only = %d %s, want 200", rr.Code, rr.Body.String())
	}

	content, position := f.row(t, id)
	if position != 7 {
		t.Errorf("after a content-only PATCH the row's position = %v, want 7 unchanged — an absent "+
			"position was written as 0 and moved the block to the top of the page", position)
	}
	if content != edited {
		t.Errorf("after a content-only PATCH the row's content = %q, want %q — the field that WAS "+
			"sent must still be written", content, edited)
	}
}

// [BOTH-FIELDS] the must-stay-green companion: a PATCH that names both still writes both. Without
// it, "keep what was not sent" could be satisfied by a route that writes NOTHING.
func TestBlockPatch_BothFields_WriteBoth_RealPG(t *testing.T) {
	f := newPatchFixture(t)
	id := f.seedBlock(t, seededContent, 3)

	const edited = `{"type":"doc","text":"rewritten"}`
	body, err := json.Marshal(map[string]any{"content": edited, "position": 4.5})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rr := f.do(t, asBlockUser(http.MethodPatch, "/v1/blocks/"+id, "alice@corp.com", string(body))); rr.Code != http.StatusOK {
		t.Fatalf("PATCH both = %d %s, want 200", rr.Code, rr.Body.String())
	}
	content, position := f.row(t, id)
	if content != edited || position != 4.5 {
		t.Errorf("PATCH {content, position} left the row at {content:%q position:%v}, want {%q, 4.5} "+
			"— the route must still write what it is given", content, position, edited)
	}
}

// [CROSS-TENANT] the workspace boundary, re-asserted THROUGH the new signature.
//
// ⚠ IT PINS THE ROUTE'S DENIAL, NOT THE STORE'S IN-METHOD GATE, AND THAT DISTINCTION IS MEASURED
// RATHER THAN REASONED. My first prediction was that deleting `assertInWorkspaces` from
// UpdateInWorkspaces would red this case. Control C5 says otherwise: it stays GREEN, because
// Alice is refused by the ROUTE enforcer (blockEnf → PageResolverFromBlock → a page lookup already
// scoped to her workspaces) and the request 404s before the handler — and therefore before the
// store — is reached. The store's own gate is pinned by failclosed_gate_test.go, which calls the
// store directly and DOES red under C5. Two different claims about two different layers; a
// control is the only thing that tells them apart, and without one this comment would have
// asserted coverage this test does not have.
func TestBlockPatch_CrossTenant_Is404_RealPG(t *testing.T) {
	d := testutil.New(t)
	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pageB := d.Page(t, wsB, bob, "B's doc")
	chain := newBlockChain(t, d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	// Bob seeds a block on his own page through the shipped route.
	rr := do(asBlockUser(http.MethodPost, "/v1/pages/"+pageB+"/blocks", "bob@corp.com",
		`{"type":"paragraph","content":"`+`{}`+`","position":1}`))
	if rr.Code != http.StatusCreated {
		t.Fatalf("Bob seeding his own block = %d %s, want 201 — the anchor failed, so the denial "+
			"below would not be about tenancy", rr.Code, rr.Body.String())
	}
	var seeded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &seeded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_ = alice

	// Alice, a member of A only, must not reach it.
	if rr := do(asBlockUser(http.MethodPatch, "/v1/blocks/"+seeded.ID, "alice@corp.com",
		`{"content":"{\"t\":9}"}`)); rr.Code != http.StatusNotFound {
		t.Errorf("Alice→B's block PATCH = %d, want 404 (never leak existence across tenants)", rr.Code)
	}
	// And the row is untouched.
	var content string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT content FROM blocks WHERE id=$1`, seeded.ID).Scan(&content); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if content != `{}` {
		t.Errorf("B's block content = %q after Alice's refused PATCH, want %q", content, `{}`)
	}
}
