package database_test

// A3 — intra-workspace TIER enforcement for inline databases. All actors are members of the SAME
// workspace W (SEC-4 L2 cross-tenant is closed separately, see sec4_l2_test.go). The bug: the
// /databases/{dbID}/* routes were gated by workspace MEMBERSHIP only, never the edit/view TIER — so a
// view-only member could mutate schema / rows / views, a write the model reserves for AccessEdit
// (every other resource — pages, blocks — already enforces this). RED (dbID routes ungated): the
// viewer-write asserts below FAIL (the store happily writes, 200/201). GREEN (dbEnf.Require wired via
// the db→page resolver): the SAME asserts pass — viewer writes 403, viewer reads 200, editor/owner
// writes succeed, a foreign-workspace member gets 404 (no oracle). Only the handler gating changes
// red→green; the assertions are the contract.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/database"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const tierSecret = "sec4-test-gateway-secret-0123456789"

// tierChain mounts ONLY the database handler behind the real gateway+authz middleware and the real
// enforcers, wired exactly as main.go does — pageEnf for the page-scoped create, dbEnf (db→page
// resolver) for every /databases/{dbID}/* route.
func tierChain(d *testutil.DB) http.Handler {
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
	dbPageLooker := func(ctx context.Context, dbID string) (string, permission.PageMeta, error) {
		var pageID string
		if err := d.Pool.QueryRow(ctx, `SELECT page_id FROM databases WHERE id=$1`, dbID).Scan(&pageID); err != nil {
			return "", permission.PageMeta{}, err
		}
		md, err := pageLooker(ctx, pageID)
		if err != nil {
			return "", permission.PageMeta{}, err
		}
		return pageID, md, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	dbEnf := permission.NewEnforcer(permStore, permission.PageResolverFromDatabase("dbID", dbPageLooker, permStore))

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(tierSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		database.NewHandler(database.NewStore(d.Pool)).WithAccess(pageEnf, dbEnf).Mount(r)
	})
	return r
}

func tierReq(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", tierSecret)
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

func TestA3_DatabaseTierEnforcement(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")   // space/page creator → admin
	viewer := d.Member(t, W, "viewer@corp.com") // view grant on the page → must NOT write the db
	editor := d.Member(t, W, "editor@corp.com") // edit grant on the page → may write the db

	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "S", Slug: "s-" + owner[len(owner)-6:], CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	pg, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sp.ID, WorkspaceID: W, Title: "P", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}
	grant := func(subject string, lvl permission.AccessLevel) {
		if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
			ResourceType: permission.ResourcePage, ResourceID: pg.ID, SubjectType: "member",
			SubjectID: subject, Access: lvl, WorkspaceID: W, GrantedBy: owner,
		}); err != nil {
			t.Fatalf("grant: %v", err)
		}
	}
	grant(viewer, permission.AccessView)
	grant(editor, permission.AccessEdit)

	dbStore := database.NewStore(d.Pool)
	db, err := dbStore.CreateDatabase(ctx, database.Database{PageID: pg.ID, WorkspaceID: W, Name: "D", Schema: []database.ColumnDef{}})
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}
	row, err := dbStore.CreateRow(ctx, database.Row{DatabaseID: db.ID, Values: map[string]any{"k": "v"}}, []string{W})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	view, err := dbStore.CreateView(ctx, database.DatabaseView{DatabaseID: db.ID, Name: "V"}, []string{W})
	if err != nil {
		t.Fatalf("seed view: %v", err)
	}

	chain := tierChain(d)
	code := func(r *http.Request) int { rr := httptest.NewRecorder(); chain.ServeHTTP(rr, r); return rr.Code }
	wrote := func(c int) bool { return c == http.StatusOK || c == http.StatusCreated }
	base := "/v1/databases/" + db.ID

	// ── RED: a view-tier member must be REFUSED (403) on EVERY inline-database write. ──
	writes := []struct {
		name, method, path, body string
	}{
		{"CreateRow", http.MethodPost, base + "/rows", `{"values":{"x":1}}`},
		{"UpdateSchema", http.MethodPatch, base + "/schema", `{"schema":[{"id":"c1","name":"Col","type":"text"}]}`},
		{"CreateView", http.MethodPost, base + "/views", `{"name":"viewer view"}`},
		{"UpdateRow", http.MethodPatch, base + "/rows/" + row.ID, `{"values":{"k":"hacked"}}`},
		{"DeleteRow", http.MethodDelete, base + "/rows/" + row.ID, ``},
		{"UpdateView", http.MethodPatch, base + "/views/" + view.ID, `{"name":"hacked"}`},
	}
	for _, w := range writes {
		if c := code(tierReq(w.method, w.path, "viewer@corp.com", w.body)); c != http.StatusForbidden {
			t.Errorf("viewer %s = %d, want 403 (view-tier cannot mutate an inline database)", w.name, c)
		}
	}

	// ── Reads are allowed at View: the viewer can still see the database, its rows, its views. ──
	for _, rd := range []struct{ name, path string }{
		{"GetDatabase", base}, {"ListRows", base + "/rows"}, {"ListViews", base + "/views"},
	} {
		if c := code(tierReq(http.MethodGet, rd.path, "viewer@corp.com", "")); c != http.StatusOK {
			t.Errorf("viewer %s = %d, want 200 (reads allowed at View)", rd.name, c)
		}
	}

	// ── POSITIVE controls — the gate must not over-block editors/owners. ──
	if c := code(tierReq(http.MethodPost, base+"/rows", "editor@corp.com", `{"values":{"k":"e"}}`)); !wrote(c) {
		t.Errorf("editor CreateRow = %d, want 200/201 (edit grant)", c)
	}
	if c := code(tierReq(http.MethodPatch, base+"/schema", "owner@corp.com", `{"schema":[{"id":"c1","name":"Col","type":"text"}]}`)); c != http.StatusOK {
		t.Errorf("owner UpdateSchema = %d, want 200 (creator=admin)", c)
	}
	if c := code(tierReq(http.MethodPost, base+"/views", "editor@corp.com", `{"name":"editor view"}`)); !wrote(c) {
		t.Errorf("editor CreateView = %d, want 200/201", c)
	}

	// ── Composition with SEC-4 L2: a member of a DIFFERENT workspace gets 404 (no existence oracle),
	//    NOT 403 — the db→page resolver's page lookup is scoped to the caller's verified workspaces. ──
	W2 := d.Workspace(t)
	d.Member(t, W2, "mallory@corp.com")
	if c := code(tierReq(http.MethodPatch, base+"/schema", "mallory@corp.com", `{"schema":[]}`)); c != http.StatusNotFound {
		t.Errorf("cross-tenant UpdateSchema = %d, want 404 (composes with L2, no 403 oracle)", c)
	}
}
