package templatelib_test

// THE {wsID} IN `/v1/workspaces/{wsID}/template-library` IS DECORATIVE ON THREE OF THE FOUR ROUTES.
//
// ⚠⚠ MEASURED, NOT READ, AND NOT A GENERALISATION FROM THE SIBLING FIX. #166 found this exact shape
// in internal/customdomain and repaired it there; the question this file answers is whether the
// class was still armed one package over. It was. Driven through the shipped chain (chi /v1 +
// gatewayauth + authz, transit proof + email claim, never a hand-made member id) on real Postgres,
// with ONE caller holding memberships in TWO workspaces:
//
//	GET    /v1/workspaces/{A}/template-library              → A's custom templates AND B's
//	POST   /v1/workspaces/{A}/template-library/{idB}/use    → 201, instantiates B's template
//	DELETE /v1/workspaces/{A}/template-library/{idB}        → 200, deletes B's template
//
// `FromPage` is the one route that had noticed: it reads {wsID} and runs it through
// authz.AuthorizeWorkspace before that workspace owns the new template (handler.go:98-106) — the
// same asymmetry customdomain had, where `Create` alone authorized the path id.
//
// ⚠ THIS IS NOT A CROSS-TENANT LEAK AND MUST NOT BE REPORTED AS ONE. `authz.WorkspaceIDs` is the
// VERIFIED membership set, so every row reachable this way belongs to a workspace the caller is
// genuinely a member of. A stranger reaches nothing — [FOREIGN-WS-REFUSED] below pins that, and it
// is the control that stops this finding being mis-sold. The defect is SCOPE: the gate names ONE
// workspace and the response carries MORE.
//
// ⚠ WHY IT IS WORTH A MERGE RATHER THAN A NOTE. The consequence is a destructive write the UI
// cannot warn about, and the wiring is the same one that made customdomain's version dangerous:
// `frontend/src/pages/TemplateGallery.tsx:43` caches the list under
// `queryKey: ["templates", workspaceID, …]` — a claim about ONE workspace — and line 126 hands
// every non-built-in row an `onDelete` wired to `remove.mutate(t.id)`. The gallery NEVER renders
// `workspace_id`, though `api/templates.ts:22` carries it on the type. So workspace B's custom
// template sits in workspace A's gallery indistinguishable from A's own, one click from deletion,
// and `Store.Delete` is a hard `DELETE FROM library_templates` — the page content it was minted
// from survives, the template does not.
//
// ⚠ WHY NOTHING SAW IT. Every existing test in this package is single-workspace: `tier_test.go`
// seeds one W and varies the CALLER's tier, `usetwice_realpg_test.go` and the two use_count files
// use one ws each, and `store_test.go` is pgxmock asserting the SQL carries `workspace_id = ANY($n)`
// — which it does, correctly, for its own purpose. The set handed IN is the handler's decision, and
// no test in this package had ever built a caller with TWO memberships. Unobservable with one
// workspace BY CONSTRUCTION.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/testutil"
)

type libScope struct {
	srv  http.Handler
	d    *testutil.DB
	wsA  string
	wsB  string
	wsC  string
	mail string
	memA string
	memB string
}

// newLibScope seeds ONE caller with memberships in TWO workspaces plus a third she is not in.
// That second membership is the whole instrument — with one workspace the defect is invisible.
func newLibScope(t *testing.T) *libScope {
	t.Helper()
	d := testutil.New(t)
	f := &libScope{d: d, srv: tierChain(d), mail: "alice@example.com"}
	f.wsA = d.Workspace(t)
	f.wsB = d.Workspace(t)
	f.wsC = d.Workspace(t) // alice is deliberately NOT a member of C
	f.memA = d.Member(t, f.wsA, f.mail)
	f.memB = d.Member(t, f.wsB, f.mail)
	d.Member(t, f.wsC, "mallory@example.com")
	return f
}

func (f *libScope) as(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	f.srv.ServeHTTP(rr, tierReq(method, url, f.mail, body))
	return rr.Code, rr.Body.String()
}

// mintTemplate creates a custom template THROUGH THE REAL from-page ROUTE, so the fixture itself
// exercises the one route that was already correct. A t.Fatalf here is a SETUP failure, not a
// caught mutation — the control harness scores by the named failing test AND its tag.
func (f *libScope) mintTemplate(t *testing.T, wsID, member, name string) templatelib.LibraryTemplate {
	t.Helper()
	src := f.d.Page(t, wsID, member, name+" Source")
	code, body := f.as(t, http.MethodPost,
		"/v1/workspaces/"+wsID+"/template-library/from-page",
		`{"page_id":"`+src+`","name":"`+name+`","description":"d","category":"general"}`)
	if code != http.StatusCreated {
		t.Fatalf("[SETUP] mint template %q in %s: %d %s", name, wsID, code, body)
	}
	var out templatelib.LibraryTemplate
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("[SETUP] decode minted template: %v (body %s)", err, body)
	}
	return out
}

// templateRow reads Postgres DIRECTLY — never Store.List or Store.GetByID, both of which are under
// measurement here and could be moved by the same edit.
func (f *libScope) templateRow(t *testing.T, id string) (workspaceID string, found bool) {
	t.Helper()
	err := f.d.Pool.QueryRow(context.Background(),
		`SELECT workspace_id FROM library_templates WHERE id = $1`, id).Scan(&workspaceID)
	if err != nil {
		return "", false
	}
	return workspaceID, true
}

// seedSpace makes a target for Use. Non-private, so every workspace member clears AccessEdit and
// the Use assertions measure SCOPE rather than a permission tier.
func (f *libScope) seedSpace(t *testing.T, wsID, member, slug string) string {
	t.Helper()
	sp, err := space.NewStore(f.d.Pool).Create(context.Background(), model.Space{
		WorkspaceID: wsID, Name: "Target " + slug, Slug: slug + "-" + member[len(member)-8:],
		CreatedBy: member,
	})
	if err != nil {
		t.Fatalf("[SETUP] seed space in %s: %v", wsID, err)
	}
	return sp.ID
}

func listTemplates(t *testing.T, body string) []templatelib.LibraryTemplate {
	t.Helper()
	var out []templatelib.LibraryTemplate
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode list: %v (body %s)", err, body)
	}
	return out
}

func hasTemplate(ts []templatelib.LibraryTemplate, name string) bool {
	for _, tm := range ts {
		if tm.Name == name {
			return true
		}
	}
	return false
}

// ─── LIST ───────────────────────────────────────────

// TestLibList_IsScopedToThePathWorkspace_RealPG is the finding. A caller in two workspaces asks for
// ONE workspace's library and is served both workspaces' custom templates.
func TestLibList_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newLibScope(t)
	f.mintTemplate(t, f.wsA, f.memA, "Alpha Only")
	f.mintTemplate(t, f.wsB, f.memB, "Beta Only")

	code, body := f.as(t, http.MethodGet, "/v1/workspaces/"+f.wsA+"/template-library", "")
	// ⚠ THIS IS AN ASSERTION, NOT SETUP, AND THE CONTROL RUN IS WHY. It was `t.Fatalf("[SETUP] …")`
	// on the first cut, and control C5 (scopeFor refuses everything) scored a catch tagged [SETUP]
	// — a fixture-failure tag standing in for the over-refusal assertion the harness was trying to
	// exercise. Asking about a workspace you ARE a member of must answer 200; that is the positive
	// control for the narrowing below, and it needs a name of its own.
	if code != http.StatusOK {
		t.Fatalf("[LIB-LIST-OWN-WORKS] GET workspace A's OWN library answered %d %s — expected 200; "+
			"a scope fix that refuses the caller's own workspace is not a fix", code, body)
	}
	got := listTemplates(t, body)

	// ── LIVENESS FLOOR. "B's template is absent from A's library" is satisfied by an empty list,
	// and an empty list is what a broken scope filter, a broken fixture or a 404'd route all
	// produce. This fails the run before that can happen. It must be the CUSTOM template, not a
	// built-in: built-ins are workspace-less and are returned whatever the scope is, so counting
	// them would leave the floor standing on rows the fix cannot affect.
	if !hasTemplate(got, "Alpha Only") {
		t.Fatalf("[LIB-LIST-OWN-VISIBLE] workspace A's OWN custom template is missing from its own "+
			"library (%d rows) — every scoping assertion below would pass vacuously", len(got))
	}

	if hasTemplate(got, "Beta Only") {
		t.Errorf("[LIB-LIST-SCOPED] GET /v1/workspaces/%s/template-library served workspace B's "+
			"custom template. List passes authz.WorkspaceIDs (the caller's WHOLE membership set) to "+
			"Store.List and never reads the {wsID} it was asked about; TemplateGallery.tsx caches "+
			"this under queryKey [\"templates\", workspaceID] and gives every non-built-in row a "+
			"delete button that never shows which workspace owns it. Got %d rows: %s",
			f.wsA, len(got), body)
	}
}

// TestLibList_TheOtherWorkspaceStillListsItsOwn_RealPG is the positive control for the assertion
// above: "scoped" must not be achieved by serving nobody anything.
func TestLibList_TheOtherWorkspaceStillListsItsOwn_RealPG(t *testing.T) {
	f := newLibScope(t)
	f.mintTemplate(t, f.wsA, f.memA, "Alpha Only")
	f.mintTemplate(t, f.wsB, f.memB, "Beta Only")

	code, body := f.as(t, http.MethodGet, "/v1/workspaces/"+f.wsB+"/template-library", "")
	if code != http.StatusOK {
		t.Fatalf("[LIB-LIST-B-OWN-WORKS] GET workspace B's OWN library answered %d %s — expected 200",
			code, body)
	}
	got := listTemplates(t, body)
	if !hasTemplate(got, "Beta Only") {
		t.Errorf("[LIB-LIST-B-OWN-VISIBLE] workspace B's own custom template is missing from B's "+
			"library — a scope fix that serves nothing is not a fix. Got %d rows: %s", len(got), body)
	}
	if hasTemplate(got, "Alpha Only") {
		t.Errorf("[LIB-LIST-SCOPED-B] workspace B's library carried workspace A's template: %s", body)
	}
	// The built-ins are workspace-less and must survive the narrowing — scoping the CUSTOM tier
	// must not silently take the gallery's whole catalogue with it.
	if !hasTemplate(got, templatelib.Builtins()[0].Name) {
		t.Errorf("[LIB-LIST-BUILTINS-SURVIVE] the built-in tier vanished from a scoped list — "+
			"built-ins have no workspace_id and belong in every workspace's gallery. Got: %s", body)
	}
}

// ─── DELETE ─────────────────────────────────────────

// TestLibDelete_IsScopedToThePathWorkspace_RealPG is the destructive half. This is the click a user
// makes in workspace A's gallery on a row that belongs to B.
func TestLibDelete_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newLibScope(t)
	b := f.mintTemplate(t, f.wsB, f.memB, "Beta Only")

	// ── LIVENESS FLOOR. The row must EXIST and belong to B before "the delete was refused" can
	// mean anything — refusing to delete a row that was never there is trivially harmless.
	if ws, ok := f.templateRow(t, b.ID); !ok || ws != f.wsB {
		t.Fatalf("[LIB-DELETE-ROW-LIVE-FIRST] B's template row is not present as B's before the "+
			"delete (found=%v ws=%q) — the refusal assertion below would be vacuous", ok, ws)
	}

	code, body := f.as(t, http.MethodDelete,
		"/v1/workspaces/"+f.wsA+"/template-library/"+b.ID, "")
	if code != http.StatusNotFound {
		t.Errorf("[LIB-DELETE-SCOPED] DELETE /v1/workspaces/%s/template-library/%s removed a "+
			"template owned by workspace B and answered %d %s — expected 404. The handler passes "+
			"the caller's whole membership set to Store.Delete and never reads {wsID}",
			f.wsA, b.ID, code, body)
	}

	// The status code is the smaller half. THE ROW IS THE CLAIM.
	if _, ok := f.templateRow(t, b.ID); !ok {
		t.Errorf("[LIB-DELETE-ROW-SURVIVES] workspace B's library_templates row was DELETED " +
			"through workspace A's URL — and nothing in the gallery that offered the button showed " +
			"it belonged to B")
	}
}

// TestLibDelete_OwnWorkspaceStillWorks_RealPG is the positive control: refusing EVERY delete would
// satisfy the test above.
func TestLibDelete_OwnWorkspaceStillWorks_RealPG(t *testing.T) {
	f := newLibScope(t)
	b := f.mintTemplate(t, f.wsB, f.memB, "Beta Only")

	code, body := f.as(t, http.MethodDelete,
		"/v1/workspaces/"+f.wsB+"/template-library/"+b.ID, "")
	if code != http.StatusOK {
		t.Fatalf("[LIB-DELETE-OWN-WORKS] deleting B's template through B's own URL answered %d %s "+
			"— expected 200; a scope fix that refuses everything is not a fix", code, body)
	}
	if _, ok := f.templateRow(t, b.ID); ok {
		t.Errorf("[LIB-DELETE-OWN-REALLY-GONE] the route answered 200 but the row is still there — " +
			"a delete that reports success and removes nothing is the quieter lie")
	}
}

// ─── USE ────────────────────────────────────────────

// TestLibUse_IsScopedToThePathWorkspace_RealPG. Use COPIES the template's content into a new page,
// so the wrong-workspace path reproduces B's document through A's URL.
func TestLibUse_IsScopedToThePathWorkspace_RealPG(t *testing.T) {
	f := newLibScope(t)
	b := f.mintTemplate(t, f.wsB, f.memB, "Beta Only")
	target := f.seedSpace(t, f.wsB, f.memB, "usescope")

	code, body := f.as(t, http.MethodPost,
		"/v1/workspaces/"+f.wsA+"/template-library/"+b.ID+"/use",
		`{"space_id":"`+target+`"}`)
	if code != http.StatusNotFound {
		t.Errorf("[LIB-USE-SCOPED] POST /v1/workspaces/%s/template-library/%s/use instantiated a "+
			"template owned by workspace B and answered %d %s — expected 404. The template read is "+
			"scoped to the caller's whole membership set, never to the {wsID} in the path",
			f.wsA, b.ID, code, body)
	}

	// THE PAGE IS THE CLAIM: a 404 that still wrote the page is the shape this repo already caught
	// once (a counter that moved on a refused create).
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pages WHERE space_id = $1`, target).Scan(&n); err != nil {
		t.Fatalf("count pages in target space: %v", err)
	}
	if n != 0 {
		t.Errorf("[LIB-USE-NO-PAGE] %d page(s) were created from workspace B's template through "+
			"workspace A's URL", n)
	}
}

// TestLibUse_OwnWorkspaceStillWorks_RealPG is the positive control for the assertion above.
func TestLibUse_OwnWorkspaceStillWorks_RealPG(t *testing.T) {
	f := newLibScope(t)
	b := f.mintTemplate(t, f.wsB, f.memB, "Beta Only")
	target := f.seedSpace(t, f.wsB, f.memB, "useown")

	code, body := f.as(t, http.MethodPost,
		"/v1/workspaces/"+f.wsB+"/template-library/"+b.ID+"/use",
		`{"space_id":"`+target+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("[LIB-USE-OWN-WORKS] using B's template through B's own URL answered %d %s — "+
			"expected 201", code, body)
	}
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pages WHERE space_id = $1`, target).Scan(&n); err != nil {
		t.Fatalf("count pages in target space: %v", err)
	}
	if n != 1 {
		t.Errorf("[LIB-USE-OWN-REALLY-CREATED] the route answered 201 but %d page(s) exist in the "+
			"target space, want 1 — body %s", n, body)
	}
}

// TestLibUse_BuiltinsStayUsableInEveryWorkspace_RealPG. Built-ins are workspace-less by design
// (store.go short-circuits them BEFORE any scoped query). Narrowing the custom tier's scope must
// not take them with it — that would break the gallery's whole catalogue for every workspace.
func TestLibUse_BuiltinsStayUsableInEveryWorkspace_RealPG(t *testing.T) {
	f := newLibScope(t)
	target := f.seedSpace(t, f.wsA, f.memA, "usebuiltin")

	code, body := f.as(t, http.MethodPost,
		"/v1/workspaces/"+f.wsA+"/template-library/builtin-rfc/use",
		`{"space_id":"`+target+`"}`)
	if code != http.StatusCreated {
		t.Errorf("[LIB-USE-BUILTIN-WORKS] using a BUILT-IN template in the caller's own workspace "+
			"answered %d %s — expected 201; built-ins have no workspace_id and a scope fix must not "+
			"make them unreachable", code, body)
	}
}

// ─── MUST-STAY-GREEN: the tenancy boundary itself ───

// TestLibForeignWorkspaceIsStillRefused_RealPG pins what this finding is NOT. wsC is a workspace
// alice has no membership in. If any of these ever serves data, the defect above has been
// mis-described and the real one is a cross-tenant leak.
//
// ⚠ THE LIST HALF IS RED BEFORE THE FIX TOO, AND IT IS A DIFFERENT STATEMENT FROM THE ONE ABOVE:
// today `/workspaces/{C}/template-library` answers 200 with ALICE'S OWN templates, because nothing
// on that route reads {wsID} at all. No row of C's escapes — C has none — so this is not a leak;
// it is the same decorative-path-param defect seen from the other side, and it is why the fix has
// to authorize {wsID} rather than merely intersect with it.
func TestLibForeignWorkspaceIsStillRefused_RealPG(t *testing.T) {
	f := newLibScope(t)
	a := f.mintTemplate(t, f.wsA, f.memA, "Alpha Only")

	code, body := f.as(t, http.MethodGet, "/v1/workspaces/"+f.wsC+"/template-library", "")
	if code != http.StatusNotFound {
		t.Errorf("[FOREIGN-WS-REFUSED] GET /v1/workspaces/%s/template-library — a workspace the "+
			"caller is NOT a member of — answered %d %s, expected 404", f.wsC, code, body)
	}

	// from-page was already correct; this pins it stays correct.
	src := f.d.Page(t, f.wsA, f.memA, "Foreign Source")
	code, body = f.as(t, http.MethodPost,
		"/v1/workspaces/"+f.wsC+"/template-library/from-page",
		`{"page_id":"`+src+`","name":"Planted","description":"d","category":"general"}`)
	if code != http.StatusNotFound {
		t.Errorf("[FOREIGN-WS-NO-PLANT] creating a template in a workspace the caller is not a "+
			"member of answered %d %s — expected 404", code, body)
	}
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM library_templates WHERE workspace_id = $1`, f.wsC).Scan(&n); err != nil {
		t.Fatalf("count wsC templates: %v", err)
	}
	if n != 0 {
		t.Errorf("[FOREIGN-WS-NO-ROW] %d library_templates row(s) landed in a workspace the caller "+
			"is not a member of", n)
	}

	// And A's own template is untouched by any of it — the liveness floor for this whole test.
	if ws, ok := f.templateRow(t, a.ID); !ok || ws != f.wsA {
		t.Errorf("[FOREIGN-WS-OWN-INTACT] workspace A's template is gone or moved (found=%v ws=%q) "+
			"— the refusals above would be measuring an empty database", ok, ws)
	}
}
