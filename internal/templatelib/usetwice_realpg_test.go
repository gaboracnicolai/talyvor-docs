package templatelib_test

// THE OTHER DOOR ONTO THE SAME COLLISION — using one template twice in one space.
//
// A template's page title is the TEMPLATE'S NAME (store.go: `Title: t.Name`), so it is fixed by
// the library, not chosen per use. `pages` carries `UNIQUE (space_id, slug)` and Store.Create
// derived the slug from that title with no uniqueness handling, so the SECOND instantiation of any
// template into a space failed. Measured through the shipped route before the fix:
//
//	POST /v1/workspaces/{ws}/template-library/builtin-rfc/use {"space_id":…} -> 201
//	POST /v1/workspaces/{ws}/template-library/builtin-rfc/use {"space_id":…} -> 400
//	  {"error":"page: insert: ERROR: duplicate key value violates unique constraint
//	            \"pages_space_id_slug_key\" (SQLSTATE 23505)"}
//
// A template library whose templates can be used once per space is not a template library. This
// case is here rather than only in internal/page because the finding is that the collision reached
// EVERY door into Store.Create — fixing it in the store is what closes all of them at once, and a
// guard that only ever drives the REST door would not say so.
//
// ⚠ THIS FILE ALSO PINS A SECOND THING THE PRE-FIX RUN EXPOSED: the built-in use counter was bumped
// BEFORE the page was created, so the failed second use still moved it — two "uses" recorded, one
// page. That half is reported in the queue and NOT fixed here (one merge per finding); the
// assertion below is on the PAGES, which is the half this change owns.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

func TestUseTemplate_TwiceIntoTheSameSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	chain := tierChain(d)
	ctx := context.Background()

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: ws, Name: "Target", Slug: "ut-" + owner[len(owner)-8:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}

	use := func() (int, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, tierReq("POST",
			"/v1/workspaces/"+ws+"/template-library/builtin-rfc/use",
			"owner@corp.com", `{"space_id":"`+sp.ID+`"}`))
		return rr.Code, rr.Body.String()
	}

	if code, body := use(); code != 201 {
		t.Fatalf("[TEMPLATE-TWICE] the FIRST use of the template failed: %d %s", code, body)
	}
	if code, body := use(); code != 201 {
		t.Errorf("[TEMPLATE-TWICE] using the same template a second time in the same space returned "+
			"%d %s — the title comes from the template, so every repeat use collides", code, body)
	}

	var n int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pages WHERE space_id = $1`, sp.ID).Scan(&n); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if n != 2 {
		t.Errorf("[TEMPLATE-TWICE] %d pages in the space after two uses of the template, want 2", n)
	}

	// Both addressable: distinct slugs, and the FIRST one keeps the plain derived slug.
	var distinct int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(DISTINCT slug) FROM pages WHERE space_id = $1`, sp.ID).Scan(&distinct); err != nil {
		t.Fatalf("count distinct slugs: %v", err)
	}
	if distinct != n {
		t.Errorf("[TEMPLATE-TWICE] %d pages share %d distinct slugs — GetBySlug addresses a page by "+
			"(space_id, slug), so a shared slug is an unreachable page", n, distinct)
	}

	// The response still points at a real page (the route's contract), for BOTH uses.
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, tierReq("POST",
		"/v1/workspaces/"+ws+"/template-library/builtin-rfc/use",
		"owner@corp.com", `{"space_id":"`+sp.ID+`"}`))
	if rr.Code != 201 {
		t.Fatalf("[TEMPLATE-TWICE] third use: %d %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var exists bool
	if err := d.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pages WHERE id = $1)`, out["page_id"]).Scan(&exists); err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !exists {
		t.Errorf("[TEMPLATE-TWICE] the route returned page_id %q and no such page exists", out["page_id"])
	}
}
