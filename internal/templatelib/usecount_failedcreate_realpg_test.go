package templatelib_test

// use_count COUNTED THE ATTEMPT, NOT THE USE — BOTH TIERS.
//
// UseTemplate bumped the counter BEFORE it asked page.Store.Create for the page, on both paths:
// `s.bumpBuiltin(templateID)` above the built-in create, and `UPDATE … use_count = use_count + 1`
// above the custom one. A create that failed therefore still moved the number, and the tile that
// renders it (frontend/src/components/TemplateCard.tsx, `{use_count} uses` under a TrendingUp icon)
// reported uses that produced no page.
//
// ⚠ THE ORIGINAL REPRO IS CLOSED AND THIS ONE REPLACES IT — that is why the counts here are 6/5 and
// not 2/1. When this was measured (queue W3.1, tab-7f5b) the SECOND use of any template into a space
// failed on `UNIQUE (space_id, slug)`, so two POSTs gave one 201, one 400 and use_count=2. #161
// (074883c) put a bounded slug retry in Store.Create, so uses two through five now succeed as
// untitled-2 … -5. The ORDERING defect was never touched by that fix — it just moved: maxSlugAttempts
// is 5, so the SIXTH use of one template into one space is the failing create today, and the counter
// still counts it. A finding whose repro a later merge invalidates is not a fixed finding, and
// re-deriving the live door is the only reason this file asserts on 6.
//
// ⚠ THE ASSERTIONS ARE ON THE COUNTER AGAINST THE PAGES, NEVER ON A LITERAL. `want` is read from
// `SELECT count(*) FROM pages`, so this file cannot drift if maxSlugAttempts changes — a threshold
// is not this test's to know. It fails if and only if the counter and the pages disagree.
//
// The sibling metric in the same call path already wrote down the governing rule, and this is the
// same sentence one package over (page/store.go, beside metrics.PagesCreated.Inc()): "AFTER the
// insert succeeded and NOT before: a refused or failed create is not a page created, and a counter
// that moves on the attempt is a different, quieter lie."
//
// NOT FIXED HERE, AND DELIBERATELY — the queue carries both as PRODUCT DECISIONS (W3.1): built-in
// use_count is CROSS-TENANT (keyed on template id alone, so one workspace is served another's
// number) and PER-PROCESS (it restarts at zero on deploy). Neither is an ordering question and
// neither is unambiguous under both readings of what the number means. This file asserts only that
// the counter counts CREATED PAGES, which is true under either reading.

import (
	"context"
	"testing"

	"encoding/json"
	"net/http/httptest"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/testutil"
)

func TestUseTemplate_UseCountCountsCreatedPagesNotAttempts_RealPG(t *testing.T) {
	d := testutil.New(t)
	chain := tierChain(d)
	ctx := context.Background()

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: ws, Name: "Target", Slug: "uc-" + owner[len(owner)-8:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}

	// A custom template too, so BOTH tiers are driven through the shipped route in one run. Its
	// name is the page title on every use, exactly as the built-in's is.
	srcPage := d.Page(t, ws, owner, "Source")
	custom, err := templatelib.NewStore(d.Pool, page.NewStore(d.Pool)).CreateFromPage(
		ctx, srcPage, ws, owner, "My Template", "desc", templatelib.CatGeneral, []string{ws})
	if err != nil {
		t.Fatalf("seed custom template: %v", err)
	}

	use := func(templateID string) (int, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, tierReq("POST",
			"/v1/workspaces/"+ws+"/template-library/"+templateID+"/use",
			"owner@corp.com", `{"space_id":"`+sp.ID+`"}`))
		return rr.Code, rr.Body.String()
	}

	// Drive each template until the route refuses one. Bounded at 12 — comfortably above
	// maxSlugAttempts (5) — so a raised bound makes this loop end, not hang.
	driveUntilRefused := func(templateID string) (created, refused int) {
		t.Helper()
		for i := 0; i < 12; i++ {
			code, body := use(templateID)
			switch {
			case code == 201:
				created++
			case code >= 400:
				refused++
				return created, refused
			default:
				t.Fatalf("[REFUSAL-REACHED] unexpected status %d from use #%d: %s", code, i+1, body)
			}
		}
		return created, refused
	}

	builtinCreated, builtinRefused := driveUntilRefused("builtin-rfc")
	if builtinRefused == 0 {
		t.Fatalf("[REFUSAL-REACHED] 12 uses of builtin-rfc into one space and the route never refused "+
			"— this test measures what the counter does on a REFUSED create, so with no refusal it "+
			"measures nothing (created=%d)", builtinCreated)
	}
	customCreated, customRefused := driveUntilRefused(custom.ID)
	if customRefused == 0 {
		t.Fatalf("[REFUSAL-REACHED] 12 uses of the custom template into one space and the route never "+
			"refused (created=%d)", customCreated)
	}

	// Pages actually in the space, per template title — the truth the counter claims to report.
	pagesTitled := func(title string) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pages WHERE space_id = $1 AND title = $2`, sp.ID, title).Scan(&n); err != nil {
			t.Fatalf("count pages titled %q: %v", title, err)
		}
		return n
	}

	// ── Built-in tier: the counter is the in-memory map, read back through the LIST route the SPA
	// actually calls, not through the store's Go accessor.
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, tierReq("GET", "/v1/workspaces/"+ws+"/template-library", "owner@corp.com", ""))
	if rr.Code != 200 {
		t.Fatalf("list template library: %d %s", rr.Code, rr.Body.String())
	}
	var listed []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		UseCount int    `json:"use_count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode library: %v (body %s)", err, rr.Body.String())
	}
	countOf := func(id string) (int, string, bool) {
		for _, tpl := range listed {
			if tpl.ID == id {
				return tpl.UseCount, tpl.Name, true
			}
		}
		return 0, "", false
	}

	builtinCount, builtinName, ok := countOf("builtin-rfc")
	if !ok {
		t.Fatalf("builtin-rfc absent from the library listing")
	}
	if wantPages := pagesTitled(builtinName); builtinCount != wantPages {
		t.Errorf("[BUILTIN-USECOUNT] the library reports use_count=%d for builtin-rfc and the space holds "+
			"%d pages from it (%d uses served 201, %d refused). A refused create is not a use: the counter "+
			"is bumped before page.Store.Create, so the failure was counted",
			builtinCount, wantPages, builtinCreated, builtinRefused)
	}

	customCount, customName, ok := countOf(custom.ID)
	if !ok {
		t.Fatalf("custom template absent from the library listing")
	}
	if wantPages := pagesTitled(customName); customCount != wantPages {
		t.Errorf("[CUSTOM-USECOUNT] the library reports use_count=%d for the custom template and the space "+
			"holds %d pages from it (%d uses served 201, %d refused). The DB bump runs before the create "+
			"on this tier too", customCount, wantPages, customCreated, customRefused)
	}

	// ── And straight from the column, so the custom half is not resting on the read path being right.
	var col int
	if err := d.Pool.QueryRow(ctx,
		`SELECT use_count FROM library_templates WHERE id = $1`, custom.ID).Scan(&col); err != nil {
		t.Fatalf("read use_count column: %v", err)
	}
	if wantPages := pagesTitled(customName); col != wantPages {
		t.Errorf("[CUSTOM-USECOUNT-COLUMN] library_templates.use_count = %d, pages from that template = %d",
			col, wantPages)
	}

	// ── The successful uses must still COUNT. Without this, moving the bump to "never" passes
	// everything above — the counter and the pages would agree at zero.
	if builtinCount == 0 {
		t.Errorf("[BUILTIN-USECOUNT-NONZERO] use_count is 0 after %d successful uses of builtin-rfc — "+
			"a counter that never moves agrees with the pages for the wrong reason", builtinCreated)
	}
	if col == 0 {
		t.Errorf("[CUSTOM-USECOUNT-NONZERO] library_templates.use_count is 0 after %d successful uses",
			customCreated)
	}
}
