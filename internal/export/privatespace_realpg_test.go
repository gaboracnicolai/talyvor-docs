package export_test

// THE SIXTH COPY OF THE PRIVATE-SPACE SEAM, AND THE ONLY ONE THAT SHIPPED THE WHOLE DOCUMENT.
//
// #78 (page /pages/search + /pages/stale), #79 (internal/ai /ask), #80 (freshness), #81 (six MCP
// tools) and #94 (analytics roll-up) each authorized the WORKSPACE and stopped. This route does
// not have that bug: it is gated, by `pageEnf.Require(permission.AccessView)`. The gate resolves
// the {pageID} URL param — so it authorizes ONE id, and answers exactly one question:
//
//	may this caller read THIS page?
//
// `?include_children=true` then appends OTHER pages to the same response. Those ids were never in
// front of any gate, because no chi resolver can gate an id that does not appear in the URL. Every
// previous copy of this seam was found by indexing on leaking STORE METHODS or on unauthorized
// WORKSPACE reads; this one is neither, which is why five sweeps went past it.
//
// ⚠⚠ MEASURED ON REAL POSTGRES BEFORE THE FIX, THROUGH THE SHIPPED CHAIN, ONE CALLER, ONE RUN:
//
//	GET /v1/spaces/{s}/pages/{child}                      as bob → 403 {"error":"forbidden"}
//	GET /v1/spaces/{s}/pages/{parent}/export
//	    ?format=markdown&include_children=true            as bob → 200, and the body carried the
//	    child's TITLE ("Q3 Layoff Plan") and its FULL RENDERED CONTENT.
//
// ALL FOUR FORMATS, measured not assumed — they share gatherPages. ⚠ AND THE PDF IS THE REASON
// THIS FILE INFLATES RATHER THAN GREPS: a raw-bytes search of the PDF said "no leak", because
// fpdf deflates its content streams. The finding was one `strings.Contains` away from being
// reported as three-formats-of-four. The inflate/unzip helpers below are instruments, so
// [ROOT-KEPT] asserts each one can SEE text in its own format — an extractor that silently
// returned nothing would make every absence assertion here vacuous.
//
// ⚠ WHAT MAKES THE CHILD UNREADABLE IS ORDINARY, NOT EXOTIC. A PRIVATE space plus a page-level
// grant on ONE page — the "share this doc" action the SharePanel exists for. resolveAccess has no
// deny grant, so inside a PUBLIC space every member already holds view on every page and there is
// nothing to leak; the private-space-plus-grant shape is the whole population, and it is the one
// customers buy private spaces FOR.
//
// THE CASES ARE ONE TEST ON PURPOSE, so a "fix" that drops every child fails here rather than
// passing quietly:
//
//	[PREMISE-200]   bob may read the parent            ← else the export 403s and absence is vacuous
//	[PREMISE-403]   bob is refused the child           ← the premise that makes this a leak
//	[LEAK-BODY]     the child's body is absent         ← the defect, per format
//	[LEAK-TITLE]    the child's title is absent        ← a title is disclosure (#94's lesson)
//	[ROOT-KEPT]     the root's body is present         ← the extractor's own positive control
//	[VISIBLE-CHILD] a GRANTED child is still exported  ← refuses "fix by dropping all children"
//	[OWNER-WHOLE]   alice still gets both children     ← the fix is a filter, not a downgrade
//	[NO-GATE-LOUD]  unwired ⇒ ErrNoPageReadGate        ← not an export quietly missing its children

import (
	"archive/zip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

const (
	hiddenBody  = "HIDDEN-CHILD-BODY-marker"
	hiddenTitle = "Q3 Layoff Plan"
	grantedBody = "GRANTED-CHILD-BODY-marker"
	rootBody    = "ROOT-BODY-marker"
)

// extractText turns one export response into searchable text. markdown/HTML are already text; the
// two binary formats are opened, because a Contains over their raw bytes answers a question about
// compression rather than about content.
func extractText(t *testing.T, format, body string) string {
	t.Helper()
	switch format {
	case "markdown", "html":
		return body
	case "docx":
		zr, err := zip.NewReader(strings.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatalf("docx is not a readable zip (%v) — the extractor cannot see content, so every "+
				"absence assertion about it would be vacuous", err)
		}
		var b strings.Builder
		for _, f := range zr.File {
			rc, oErr := f.Open()
			if oErr != nil {
				t.Fatalf("docx open %s: %v", f.Name, oErr)
			}
			raw, rErr := io.ReadAll(rc)
			_ = rc.Close()
			if rErr != nil {
				t.Fatalf("docx read %s: %v", f.Name, rErr)
			}
			b.Write(raw)
		}
		return b.String()
	case "pdf":
		// fpdf deflates its content streams. Inflate every one; a stream that is not zlib (the
		// font/metadata objects) is skipped rather than fatal.
		var b strings.Builder
		rest := body
		for {
			i := strings.Index(rest, "stream")
			if i < 0 {
				break
			}
			j := strings.Index(rest[i:], "endstream")
			if j < 0 {
				break
			}
			chunk := strings.TrimLeft(rest[i+len("stream"):i+j], "\r\n")
			if zr, err := zlib.NewReader(strings.NewReader(chunk)); err == nil {
				raw, _ := io.ReadAll(zr)
				_ = zr.Close()
				b.Write(raw)
			}
			rest = rest[i+j+len("endstream"):]
		}
		return b.String()
	}
	t.Fatalf("extractText: unknown format %q", format)
	return ""
}

type exportFixture struct {
	ws, alice, bob  string
	spaceID         string
	parent          string
	hidden, granted string
	exportH         *export.Handler
	pageH           *page.Handler
	unwiredExporter *export.Exporter
}

func newExportFixture(t *testing.T, d *testutil.DB) *exportFixture {
	t.Helper()
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice-export@example.com")
	bob := d.Member(t, ws, "bob-export@example.com")

	pageStore := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// PRIVATE, created by alice. bob is a workspace member with no space grant: the default tier
	// for him here is none, which is what makes a page-level grant the only thing he holds.
	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, 'Leadership', 'sp-leadership-export', $2, true) RETURNING id`,
		ws, alice).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	seedPage := func(title, slug, marker string, parent *string) string {
		pm := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + marker + `"}]}]}`
		var id string
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, parent_id, content, content_text)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			spaceID, ws, title, slug, alice, parent, pm, marker).Scan(&id); err != nil {
			t.Fatalf("seed page %q: %v", title, err)
		}
		return id
	}
	parent := seedPage("Board Pack", "pg-parent-export", rootBody, nil)
	hidden := seedPage(hiddenTitle, "pg-hidden-export", hiddenBody, &parent)
	granted := seedPage("Agenda", "pg-granted-export", grantedBody, &parent)

	// The ordinary share: view on the parent, and on ONE child. Nothing on the other.
	for _, id := range []string{parent, granted} {
		if err := permStore.Grant(ctx, permission.Permission{
			ResourceType: permission.ResourcePage, ResourceID: id,
			SubjectType: "member", SubjectID: bob, Access: permission.AccessView,
			WorkspaceID: ws, GrantedBy: alice,
		}); err != nil {
			t.Fatalf("grant on %s: %v", id, err)
		}
	}

	// cmd/docs/main.go's lookers, re-derived rather than imported from another test's helper: they
	// are the metadata the permission engine decides on, and borrowing a helper borrows its
	// evidence.
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
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID,
			SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))

	// Wired exactly as main.go wires it: the route enforcer AND the per-page read gate.
	exporter := export.New(pageStore, spaceStore).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))

	return &exportFixture{
		ws: ws, alice: alice, bob: bob, spaceID: spaceID,
		parent: parent, hidden: hidden, granted: granted,
		exportH:         export.NewHandler(exporter).WithAccess(pageEnf),
		pageH:           page.NewHandler(pageStore, nil).WithAccess(pageEnf, spaceEnf),
		unwiredExporter: export.New(pageStore, spaceStore),
	}
}

// get drives the REAL handlers as main.go mounts them, through chi, with the caller's memberships
// on the context exactly as authz.Middleware leaves them.
func (f *exportFixture) get(t *testing.T, memberID, email, path string) (int, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), email,
				[]authz.Membership{{WorkspaceID: f.ws, MemberID: memberID}})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Route("/v1", func(r chi.Router) {
		f.exportH.Mount(r)
		f.pageH.Mount(r)
	})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code, rr.Body.String()
}

func TestExport_IncludeChildren_DoesNotShipAPageTheCallerIsRefused_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newExportFixture(t, d)

	// ── the premises. Both are about bob and both must hold, or every absence below is vacuous.
	if code, body := f.get(t, f.bob, "bob-export@example.com",
		"/v1/spaces/"+f.spaceID+"/pages/"+f.parent); code != http.StatusOK {
		t.Fatalf("[PREMISE-200] bob cannot read the parent (%d %s) — his page grant did not take, "+
			"so the export below would be refused for a reason that has nothing to do with this "+
			"finding and every absence assertion would pass on an error page",
			code, strings.TrimSpace(body))
	}
	if code, body := f.get(t, f.bob, "bob-export@example.com",
		"/v1/spaces/"+f.spaceID+"/pages/"+f.hidden); code == http.StatusOK {
		t.Fatalf("[PREMISE-403] bob CAN read the hidden child at the by-page route (%d %s) — it is "+
			"not hidden from him, so nothing below is a leak", code, strings.TrimSpace(body))
	}

	exportPath := "/v1/spaces/" + f.spaceID + "/pages/" + f.parent + "/export?include_children=true&format="
	for _, format := range []string{"markdown", "html", "pdf", "docx"} {
		code, body := f.get(t, f.bob, "bob-export@example.com", exportPath+format)
		if code != http.StatusOK {
			t.Fatalf("export %s as bob: %d %s — he holds view on the root", format, code, strings.TrimSpace(body))
		}
		text := extractText(t, format, body)

		// [ROOT-KEPT] doubles as the extractor's positive control: an inflate/unzip that returned
		// nothing would satisfy both absence assertions below perfectly.
		if !strings.Contains(text, rootBody) {
			t.Fatalf("[ROOT-KEPT] the ROOT page's own body is missing from the %s export bob is "+
				"entitled to. Either the fix over-filtered, or this format's extractor sees "+
				"nothing — in which case [LEAK-BODY] and [LEAK-TITLE] below are vacuous for %s",
				format, format)
		}
		if strings.Contains(text, hiddenBody) {
			t.Errorf("[LEAK-BODY] the %s export handed bob the FULL CONTENT of a child page the "+
				"by-page route refuses him with 403. The route enforcer authorizes the {pageID} in "+
				"the URL; include_children appends ids nothing gated", format)
		}
		if strings.Contains(text, hiddenTitle) {
			t.Errorf("[LEAK-TITLE] the %s export named a page bob cannot open (%q). A title is "+
				"disclosure on its own — #94 measured the same thing on the analytics roll-up",
				format, hiddenTitle)
		}
		// [VISIBLE-CHILD] — the fix must be a filter, not a retreat. Emptying the child list
		// satisfies every absence assertion above.
		if !strings.Contains(text, grantedBody) {
			t.Errorf("[VISIBLE-CHILD] the %s export dropped a child bob HOLDS A VIEW GRANT ON. "+
				"include_children must still include the children he may read", format)
		}
	}

	// [OWNER-WHOLE] — alice created the space, so resolveAccess makes her admin on every page in
	// it. Her export must be unchanged: this is a visibility filter, not a feature downgrade.
	code, body := f.get(t, f.alice, "alice-export@example.com", exportPath+"markdown")
	if code != http.StatusOK {
		t.Fatalf("export as alice: %d %s", code, strings.TrimSpace(body))
	}
	for _, want := range []string{rootBody, hiddenBody, grantedBody} {
		if !strings.Contains(body, want) {
			t.Errorf("[OWNER-WHOLE] alice's export is missing %q — she is admin on every page in "+
				"the space she created, and the gate must not take content from her", want)
		}
	}
}

// [NO-GATE-LOUD] — an exporter whose gate was never wired must SAY SO, not quietly return an
// export missing its children. Same shape as analytics.ErrNoPageReadGate, and for the same reason:
// a document that silently lost half its pages is a different false statement, not a safe one.
//
// The unwired exporter here is built by the fixture from the same stores, so this is the wiring
// that is missing and nothing else.
func TestExport_UnwiredPageReadGate_IsLoud_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newExportFixture(t, d)
	ctx := authz.WithMemberships(context.Background(), "alice-export@example.com",
		[]authz.Membership{{WorkspaceID: f.ws, MemberID: f.alice}})

	if _, err := f.unwiredExporter.ToMarkdown(ctx, f.parent, []string{f.ws},
		export.ExportOptions{Format: export.FormatMD, IncludeChildren: true}); !errors.Is(err, export.ErrNoPageReadGate) {
		t.Errorf("[NO-GATE-LOUD] include_children with no gate returned err=%v, want "+
			"ErrNoPageReadGate — an export that silently drops the children it was asked for is "+
			"its own false answer", err)
	}
	// And the root-only export is untouched by the gate: it is authorized by the route.
	md, err := f.unwiredExporter.ToMarkdown(ctx, f.parent, []string{f.ws},
		export.ExportOptions{Format: export.FormatMD})
	if err != nil || !strings.Contains(md, rootBody) {
		t.Errorf("[NO-GATE-LOUD] a root-only export must not depend on the child gate: err=%v", err)
	}
}
