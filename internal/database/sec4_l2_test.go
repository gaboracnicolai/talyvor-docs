package database_test

// SEC-4 secondary L2 scoping. The 7 secondary groups are gated behind /v1 gatewayauth+authz
// (unauthenticated → 401, proven separately) but their by-id store ops are NOT scoped to the
// caller's verified workspace membership — either bare `WHERE id=$1`, or the DECEPTIVE shape where
// `AND workspace_id=$n` is fed from chi.URLParam("wsID") (attacker-controlled). So Alice, a verified
// member of workspace A ONLY, reaches workspace B's objects by id. GREEN: every op scopes to
// authz.WorkspaceIDs(ctx) → 404 for a foreign id; Bob still acts on his own; no existence oracle.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/approval"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/customdomain"
	"github.com/talyvor/docs/internal/database"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/templatelib"
	"github.com/talyvor/docs/internal/testutil"
)

const l2Secret = "sec4-test-gateway-secret-0123456789"

func l2Chain(d *testutil.DB) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(l2Secret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		approval.NewHandler(approval.NewStore(d.Pool)).Mount(r)
		permission.NewHandler(permission.NewStore(d.Pool)).Mount(r)
		database.NewHandler(database.NewStore(d.Pool)).Mount(r)
		changelog.NewHandler(changelog.NewStore(d.Pool, nil)).Mount(r)
		templatelib.NewHandler(templatelib.NewStore(d.Pool, nil)).Mount(r)
		customdomain.NewHandler(customdomain.NewStore(d.Pool), nil).Mount(r)
		export.NewHandler(export.New(page.NewStore(d.Pool), space.NewStore(d.Pool))).Mount(r)
	})
	return r
}

func l2Req(method, path, email string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(`{"decision":"approved"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", l2Secret) // valid transit proof — these callers ARE authenticated
	if email != "" {
		r.Header.Set("X-User-Email", email)
	}
	return r
}

func l2SpaceOf(t *testing.T, d *testutil.DB, pageID string) string {
	t.Helper()
	var s string
	if err := d.Pool.QueryRow(context.Background(), `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&s); err != nil {
		t.Fatalf("space_of: %v", err)
	}
	return s
}

func l2RowExists(t *testing.T, d *testutil.DB, table, id string) bool {
	t.Helper()
	var ok bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&ok); err != nil {
		t.Fatalf("exists %s: %v", table, err)
	}
	return ok
}

func TestSEC4_L2_SecondaryCrossTenant(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com") // Alice ∈ A only
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com") // Bob ∈ B only
	pageB := d.Page(t, wsB, bob, "Secret B roadmap")
	spaceB := l2SpaceOf(t, d, pageB)

	// ── Seed B's objects across the 7 groups ──
	dbB, err := database.NewStore(d.Pool).CreateDatabase(ctx, database.Database{PageID: pageB, WorkspaceID: wsB, Name: "B db", Schema: []database.ColumnDef{}})
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}
	rowB, err := database.NewStore(d.Pool).CreateRow(ctx, database.Row{DatabaseID: dbB.ID, Values: map[string]any{"k": "secret"}}, []string{wsB})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: spaceB, SubjectType: "member",
		SubjectID: bob, Access: permission.AccessAdmin, WorkspaceID: wsB, GrantedBy: bob,
	}); err != nil {
		t.Fatalf("seed permission: %v", err)
	}
	var permB string
	if err := d.Pool.QueryRow(ctx, `SELECT id FROM permissions WHERE resource_id=$1 LIMIT 1`, spaceB).Scan(&permB); err != nil {
		t.Fatalf("seed perm id: %v", err)
	}
	if _, err := approval.NewStore(d.Pool).RequestApproval(ctx, pageB, wsB, bob, []string{bob}, "review please", nil); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	entB, err := changelog.NewStore(d.Pool, nil).CreateEntry(ctx, changelog.ChangelogEntry{
		PageID: pageB, WorkspaceID: wsB, Version: "1.0.0", Title: "B secret log", Type: changelog.EntryFeature, Content: "secret", CreatedBy: bob,
	})
	if err != nil {
		t.Fatalf("seed changelog: %v", err)
	}
	tmplB, err := templatelib.NewStore(d.Pool, nil).CreateFromPage(ctx, pageB, wsB, bob, "B template", "desc", templatelib.CatGeneral, []string{wsB})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}
	cdB, err := customdomain.NewStore(d.Pool).Create(ctx, wsB, "b-secret.example.com", bob, nil)
	if err != nil {
		t.Fatalf("seed customdomain: %v", err)
	}

	chain := l2Chain(d)
	code := func(r *http.Request) int { rr := httptest.NewRecorder(); chain.ServeHTTP(rr, r); return rr.Code }
	const A = "alice@corp.com"

	// ── Alice (∈ A only) acting on B's objects — each MUST 404 (GREEN); today they succeed (RED) ──

	// (a) database.DeleteRow — bare WHERE id=$1
	if c := code(l2Req(http.MethodDelete, "/v1/databases/"+dbB.ID+"/rows/"+rowB.ID, A)); c != http.StatusNotFound {
		t.Errorf("(a) Alice DELETE B's db row = %d, want 404", c)
	}
	if !l2RowExists(t, d, "database_rows", rowB.ID) {
		t.Errorf("(a) B's db row was DESTROYED cross-tenant")
	}

	// (b) permission.RevokeByID — bare WHERE id=$1 (the permission system leaking its own grants)
	if c := code(l2Req(http.MethodDelete, "/v1/spaces/"+spaceB+"/permissions/"+permB, A)); c != http.StatusNotFound {
		t.Errorf("(b) Alice REVOKE B's permission = %d, want 404", c)
	}
	if !l2RowExists(t, d, "permissions", permB) {
		t.Errorf("(b) B's permission grant was REVOKED cross-tenant")
	}

	// (c) approval — Latest reads B's request (disclosure); Publish flips B's page doc_status (tamper)
	if c := code(l2Req(http.MethodGet, "/v1/spaces/"+spaceB+"/pages/"+pageB+"/approval", A)); c != http.StatusNotFound {
		t.Errorf("(c) Alice GET B's approval = %d, want 404 (disclosure)", c)
	}
	if c := code(l2Req(http.MethodPost, "/v1/spaces/"+spaceB+"/pages/"+pageB+"/publish", A)); c != http.StatusNotFound {
		t.Errorf("(c) Alice PUBLISH B's page (doc_status write) = %d, want 404 (tamper)", c)
	}

	// (d) changelog.DeleteEntry — bare WHERE id=$1
	if c := code(l2Req(http.MethodDelete, "/v1/spaces/"+spaceB+"/pages/"+pageB+"/changelog/entries/"+entB.ID, A)); c != http.StatusNotFound {
		t.Errorf("(d) Alice DELETE B's changelog entry = %d, want 404", c)
	}
	if !l2RowExists(t, d, "changelog_entries", entB.ID) {
		t.Errorf("(d) B's changelog entry was DESTROYED cross-tenant")
	}

	// (e) export — reads B's source page by id and streams it
	if c := code(l2Req(http.MethodGet, "/v1/spaces/"+spaceB+"/pages/"+pageB+"/export?format=markdown", A)); c != http.StatusNotFound {
		t.Errorf("(e) Alice EXPORT B's page = %d, want 404 (full-content exfiltration)", c)
	}

	// customdomain ADMIN Verify — bare WHERE id=$1 (do NOT touch the public renderer)
	if c := code(l2Req(http.MethodPost, "/v1/workspaces/"+wsA+"/custom-domains/"+cdB.ID+"/verify", A)); c != http.StatusNotFound {
		t.Errorf("(f) Alice VERIFY B's custom domain = %d, want 404", c)
	}

	// ── THE DECEPTIVE SHAPE: templatelib.Delete scopes via chi.URLParam("wsID"). Alice puts B's
	// wsID in the URL → the `AND workspace_id=$2` matches ($2=B from the URL) → deletes B's template
	// today. Must become 404 (scoped to Alice's VERIFIED set {A}, never the URL's B). ──
	if c := code(l2Req(http.MethodDelete, "/v1/workspaces/"+wsB+"/template-library/"+tmplB.ID, A)); c != http.StatusNotFound {
		t.Errorf("(DECEPTIVE) Alice DELETE B's template via wsID=B-in-URL = %d, want 404", c)
	}
	if !l2RowExists(t, d, "library_templates", tmplB.ID) {
		t.Errorf("(DECEPTIVE) B's template DESTROYED via the URL-param workspace scope")
	}

	// ── NO-ORACLE: a nonexistent id → same 404 as a foreign id ──
	if c := code(l2Req(http.MethodDelete, "/v1/databases/"+dbB.ID+"/rows/00000000-0000-0000-0000-000000000000", A)); c != http.StatusNotFound {
		t.Errorf("nonexistent row id = %d, want 404 (no oracle)", c)
	}

	// ── SCOPE-SOURCE: Bob (∈ B) acting on his OWN objects still succeeds ──
	const Bob = "bob@corp.com"
	if c := code(l2Req(http.MethodDelete, "/v1/spaces/"+spaceB+"/permissions/"+permB, Bob)); c != http.StatusOK {
		t.Errorf("Bob REVOKE own permission = %d, want 200 (over-blocked)", c)
	}
	if c := code(l2Req(http.MethodDelete, "/v1/databases/"+dbB.ID+"/rows/"+rowB.ID, Bob)); c != http.StatusOK {
		t.Errorf("Bob DELETE own db row = %d, want 200 (over-blocked)", c)
	}
}
