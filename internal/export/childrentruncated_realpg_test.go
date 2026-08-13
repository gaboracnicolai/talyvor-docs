package export_test

// `?include_children=true` DROPS EVERY CHILD ONCE THE SPACE HOLDS 100 OTHER PAGES, AND ANSWERS 200.
//
// gatherPages asks the store for the WHOLE SPACE and then keeps the rows whose parent_id is the
// root:
//
//	e.pages.List(ctx, page.PageFilter{SpaceID: root.SpaceID})
//
// PageFilter.Limit is left at its zero value, and page.Store.List reads that as "give me the
// default" — `if limit <= 0 { limit = 100 }` (store.go:500-503). So the Go-side parentage filter
// runs over the FIRST 100 ROWS OF THE SPACE, not over the space. The ordering decides which 100:
//
//	ORDER BY depth ASC, position ASC, created_at ASC   (store.go:525)
//
// `depth ASC` puts every top-level page ahead of every child, so this is not a tail effect: with T
// top-level pages in the space, at most 100−T of its depth-1 pages come back AT ALL, and at T=100
// none do — no matter when the children were made or where they sort among themselves.
//
// ⚠ THE PACKAGE HAS ALREADY DECIDED THIS IS UNACCEPTABLE, IN ITS OWN WORDS, ONE FILE OVER.
// Exporter.WithPageRead's doc comment says an unwired gate must be an ERROR and not "an export
// with the children quietly dropped", because "silently omitting children from a document a user
// asked to export WITH them is its own false statement, and it is the kind that gets noticed
// months later". ErrNoPageReadGate exists to honour that. The 100-row default breaks the same rule
// on the same route, for a reason nobody wrote down — no caller chose 100 here; it is the value
// List uses when a caller names none. So this needs no new judgement call: the file's own stated
// rule is the specification, and the 50MB MaxExportBytes cap (handler.go:25 → 413) is what bounds
// the response, LOUDLY, which is exactly the contrast.
//
// ⚠ AND THE PARENT SCOPE THIS READ WANTS ALREADY EXISTS AND IS ALREADY DRIVEN. PageFilter.ParentID
// reaches the SQL (`$4::text IS NULL OR parent_id = $4`) — merge #116 put it there because
// list_pages promised a parent scope its query never applied. gatherPages is the caller that wants
// exactly that scope and filters in Go instead, which is what makes the space's size decide
// whether a page's own children are in its export.
//
// MEASURED, one run, real Postgres, the shipped chain (chi → export.Handler → Exporter →
// page.Store), markdown/html/pdf/docx:
//
//	space with 2 pages besides the root    → export contains BOTH children         ([PREMISE-SMALL])
//	space with 100 further top-level pages → export contains NEITHER, status 200   ([ALL-CHILDREN])
//	root with 501 children, nothing else   → export contains 99 of them, status 200 ([EVERY-BATCH])
//
// THE TAGS:
//
//	[PREMISE-SMALL]  the same root+children export whole in a small space   ← else every absence is vacuous
//	[PREMISE-COUNT]  the crowded space really holds 100 top-level pages     ← the seeding is what it claims
//	[PREMISE-DEPTH]  the children really are depth 1 under the root         ← the shipped Create derived it
//	[ALL-CHILDREN]   every child is in the export of a crowded space        ← the defect
//	[NOT-BY-LUCK]    ... and the root is still there too                    ← refuses "fix" by dumping the space
//	[PREMISE-BATCH]  the paged root really has more children than one batch ← else EVERY-BATCH is untested
//	[EVERY-BATCH]    a root with 501 children exports all 501               ← the OTHER half of the fix
//
// [ALL-CHILDREN] and [EVERY-BATCH] are in SEPARATE tests because neither can see the other's
// mutation: with two children a one-batch read and a paging read are the same read, and with no
// crowd the space-wide default never bites. Controls E2/E3 are exactly the fixes that pass one and
// fail the other.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// crowdFillers is the number of extra TOP-LEVEL pages in the crowded space.
//
// ⚠ THE BOUNDARY IS AN INEQUALITY OVER ROWS, NOT A ROUND NUMBER, AND TWO DRAFTS OF THIS COMMENT
// GOT IT WRONG BEFORE THE CONTROLS SAID SO. List's default LIMIT is 100 ROWS and `depth ASC`
// hands them to the top level first, so with T top-level pages in the space AT MOST 100−T depth-1
// pages survive anywhere in it — the root is one of the T, and each child spends one of the
// remainder. Measured against the unfixed source (w31-exportchildren-controls-b9f1.py, B1/B2):
//
//	97 fillers → 98 top-level → both children land at rows 99 and 100 → the guard PASSES
//	98 fillers → 99 top-level → room for one of the two           → [ALL-CHILDREN] fires
//	99 fillers → 100 top-level → the limit is spent before depth 1 → every child is gone
//
// 100 fillers is one clear step past the boundary rather than sitting on it.
const crowdFillers = 100

type truncFixture struct {
	ws      string
	alice   string
	spaceID string
	root    string
	childA  string
	childB  string
	exportH *export.Handler
	pages   *page.Store
}

// newTruncFixture builds one space through the SHIPPED page.Store.Create — not raw INSERTs —
// because `depth` is DERIVED there (parent.depth + 1) and depth is the column the ORDER BY sorts
// on first. A hand-written INSERT leaves depth at its default 0 for a child too, which would make
// the crowded case fail for a different reason than production's.
func newTruncFixture(t *testing.T, d *testutil.DB, fillers int) *truncFixture {
	t.Helper()
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, fmt.Sprintf("alice-trunc-%d@example.com", fillers))

	pageStore := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, $2, $3, $4, true) RETURNING id`,
		ws, fmt.Sprintf("Handbook %d", fillers), fmt.Sprintf("sp-trunc-%d", fillers), alice,
	).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}

	mk := func(title, marker string, parent *string) string {
		p, err := pageStore.Create(ctx, model.Page{
			SpaceID:     spaceID,
			WorkspaceID: ws,
			Title:       title,
			ParentID:    parent,
			CreatedBy:   alice,
			Content: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` +
				marker + `"}]}]}`,
		})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return p.ID
	}

	root := mk("Board Pack", truncRootBody, nil)
	childA := mk("Agenda", truncChildABody, &root)
	childB := mk("Minutes", truncChildBBody, &root)

	// The crowd: ordinary top-level pages in the same space, created AFTER the children so that
	// creation order alone would have kept the children in. depth ASC is what evicts them.
	for i := 0; i < fillers; i++ {
		mk(fmt.Sprintf("Filler %03d", i), fmt.Sprintf("FILLER-BODY-%03d", i), nil)
	}

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
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	exporter := export.New(pageStore, spaceStore).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))

	return &truncFixture{
		ws: ws, alice: alice, spaceID: spaceID,
		root: root, childA: childA, childB: childB,
		exportH: export.NewHandler(exporter).WithAccess(pageEnf),
		pages:   pageStore,
	}
}

const (
	truncRootBody   = "TRUNC-ROOT-BODY-marker"
	truncChildABody = "TRUNC-CHILD-A-BODY-marker"
	truncChildBBody = "TRUNC-CHILD-B-BODY-marker"
)

func (f *truncFixture) export(t *testing.T, format string) (int, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), "alice-trunc@example.com",
				[]authz.Membership{{WorkspaceID: f.ws, MemberID: f.alice}})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Route("/v1", func(r chi.Router) { f.exportH.Mount(r) })
	rr := httptest.NewRecorder()
	rr.Body.Reset()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/spaces/"+f.spaceID+"/pages/"+f.root+"/export?include_children=true&format="+format, nil)
	r.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
}

// TestExport_IncludeChildren_PagesPastOneBatch_RealPG covers the OTHER half of the fix, and it
// exists because the crowded-space test above CANNOT see it: with two children, a read that asks
// for one batch and a read that pages until exhausted are the same read. This one gives the root
// more direct children than page.Store.List will return in a single call (its clamp is 500), so a
// fix that scopes to the parent and stops after one batch — the obvious near-miss — fails here and
// only here.
//
// Markdown only, and deliberately: gatherPages is shared by all four formats (the crowded-space
// test pins that), so re-rendering 500-odd pages into a PDF and a DOCX would buy no assertion and
// spend seconds.
func TestExport_IncludeChildren_PagesPastOneBatch_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice-paging@example.com")

	pageStore := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, 'Runbooks', 'sp-paging', $2, true) RETURNING id`,
		ws, alice).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}
	mk := func(title, marker string, parent *string) string {
		p, err := pageStore.Create(ctx, model.Page{
			SpaceID: spaceID, WorkspaceID: ws, Title: title, ParentID: parent, CreatedBy: alice,
			Content: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` +
				marker + `"}]}]}`,
		})
		if err != nil {
			t.Fatalf("create %q: %v", title, err)
		}
		return p.ID
	}

	root := mk("Runbook Index", truncRootBody, nil)
	// One more child than a single List call can return. 501 is not a threshold this test picked:
	// it is exporter.go's childBatch, which is page.Store.List's own clamp, plus one.
	const childCount = 501
	markers := make([]string, 0, childCount)
	for i := 0; i < childCount; i++ {
		m := fmt.Sprintf("PAGED-CHILD-%03d-marker", i)
		markers = append(markers, m)
		mk(fmt.Sprintf("Runbook %03d", i), m, &root)
	}

	var seeded int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pages WHERE parent_id = $1`, root).Scan(&seeded); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if seeded != childCount {
		t.Fatalf("[PREMISE-BATCH] the root has %d children, want %d — fewer than one batch plus one "+
			"would let a single-batch read pass [EVERY-BATCH] without paging at all",
			seeded, childCount)
	}

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
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	exporter := export.New(pageStore, spaceStore).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
	f := &truncFixture{
		ws: ws, alice: alice, spaceID: spaceID, root: root,
		exportH: export.NewHandler(exporter).WithAccess(pageEnf),
		pages:   pageStore,
	}

	code, body := f.export(t, "markdown")
	if code != http.StatusOK {
		t.Fatalf("paging export: %d %s", code, strings.TrimSpace(body))
	}
	if !strings.Contains(body, truncRootBody) {
		t.Fatalf("[NOT-BY-LUCK] the root's own body is missing from its export")
	}
	var missing []string
	for _, m := range markers {
		if !strings.Contains(body, m) {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		show := missing
		if len(show) > 5 {
			show = show[:5]
		}
		t.Errorf("[EVERY-BATCH] %d of %d children are missing from the export (first: %v). One "+
			"page.Store.List call returns at most 500 rows — it clamps anything larger — so reading "+
			"the children needs to continue until a short batch comes back. Stopping after the first "+
			"batch drops the rest silently, with a 200 and no marker, which is the same false "+
			"statement the space-wide default made.",
			len(missing), childCount, show)
	}
}

func TestExport_IncludeChildren_SurvivesACrowdedSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	// ── PREMISE: the identical root+children export whole when the space is small. Without this
	// the crowded case's absences prove nothing about truncation — a broken fixture, a wrong
	// marker or a 403 would read the same way.
	small := newTruncFixture(t, d, 0)
	for _, format := range []string{"markdown", "html", "pdf", "docx"} {
		code, body := small.export(t, format)
		if code != http.StatusOK {
			t.Fatalf("[PREMISE-SMALL] small-space export %s: %d %s", format, code, strings.TrimSpace(body))
		}
		text := extractText(t, format, body)
		for _, want := range []string{truncRootBody, truncChildABody, truncChildBBody} {
			if !strings.Contains(text, want) {
				t.Fatalf("[PREMISE-SMALL] %s export of a 3-page space is missing %q — the fixture, the "+
					"route or the extractor is broken, so every absence in the crowded case below would "+
					"be vacuous", format, want)
			}
		}
	}

	// ── the crowded space.
	big := newTruncFixture(t, d, crowdFillers)

	var topLevel, children, childDepth int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE parent_id IS NULL),
		        count(*) FILTER (WHERE parent_id = $2),
		        COALESCE(max(depth) FILTER (WHERE parent_id = $2), -1)
		 FROM pages WHERE space_id = $1`,
		big.spaceID, big.root,
	).Scan(&topLevel, &children, &childDepth); err != nil {
		t.Fatalf("count seeded pages: %v", err)
	}
	if topLevel != crowdFillers+1 {
		t.Fatalf("[PREMISE-COUNT] the crowded space holds %d top-level pages, want %d (%d fillers + the "+
			"root) — the crowd is what pushes the children past List's default LIMIT, so a short seed "+
			"would make [ALL-CHILDREN] pass without the defect being fixed",
			topLevel, crowdFillers+1, crowdFillers)
	}
	if children != 2 {
		t.Fatalf("[PREMISE-COUNT] the root has %d children in the database, want 2", children)
	}
	if childDepth != 1 {
		t.Fatalf("[PREMISE-DEPTH] the children are at depth %d, want 1 — `depth ASC` is the first key of "+
			"List's ORDER BY, and children that share the fillers' depth would be evicted (or kept) for "+
			"a reason production does not have", childDepth)
	}

	for _, format := range []string{"markdown", "html", "pdf", "docx"} {
		code, body := big.export(t, format)
		if code != http.StatusOK {
			t.Fatalf("crowded-space export %s: %d %s — the caller created this space and holds admin on it",
				format, code, strings.TrimSpace(body))
		}
		text := extractText(t, format, body)

		if !strings.Contains(text, truncRootBody) {
			t.Errorf("[NOT-BY-LUCK] %s export of the crowded space lost the ROOT's body — the root is "+
				"gated by the route itself and must never depend on the space's size", format)
		}
		for tag, want := range map[string]string{
			"A": truncChildABody,
			"B": truncChildBBody,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("[ALL-CHILDREN] %s export with include_children=true silently omitted child %s "+"(%q) from a space holding %d top-level pages, and answered 200. gatherPages reads "+
					"the space through page.Store.List with PageFilter.Limit unset, which List reads as "+
					"100 (store.go:500-503) and orders `depth ASC` first — so every child sorts behind "+
					"every top-level page and none of them fit. The document the user asked for came "+
					"back incomplete with no error, no header and no truncation marker.",
					format, tag, want, crowdFillers+1)
			}
		}
	}
}
