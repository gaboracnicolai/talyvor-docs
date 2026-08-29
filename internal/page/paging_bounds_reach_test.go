package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// TWO BOUNDS ON HOW MANY PAGES ONE REQUEST MAY RETURN, NEITHER OF WHICH ANYTHING NOTICED.
//
// MEASURED against a 601-page space (W3.60, tab-p3w8), each bound neutered in turn:
//
//	Store.List's clamp of 500 removed   -> GET /spaces/{id}/pages?limit=99999 returns 601 rows.
//	                                       A caller can ask for a whole space in one request.
//	the search route's truncation removed -> GET .../pages/search?q=..&limit=1 returns 4 rows and
//	                                       limit=3 returns 12 — exactly limit x searchFetchFactor.
//	                                       The OVER-FETCH leaks to the caller and the requested
//	                                       page size is ignored.
//
// ⚠ BOTH ARE REACHABLE FROM A QUERY PARAMETER WITH NO LOWER OR UPPER GUARD IN FRONT OF THEM.
// `List` parses `?limit=` with strconv.Atoi straight into PageFilter.Limit; the store clamp is the
// only ceiling. The same clamp is reached a second way, from the MCP `list_pages` tool
// (`Limit: intArg(args,"limit",20)`), so this is not a single-surface bound.
//
// ⚠ WHY THESE ARE HERE AT ALL, since W3.54 argued against them: its argument — "a fifth paging
// test adds rows and not information once F9 pins the window they all size themselves from" — was
// written BEFORE F9 was pinned. Re-measured after `d0788f2` pinned it, F9 covered NEITHER, and the
// two observables above are different from each other and from F9's.
//
// ⚠ THE OVER-FETCH IS THE POINT OF THE SECOND ONE. The search route deliberately asks the store for
// `limit * searchFetchFactor` rows so that pages the caller may not read can be dropped without
// under-filling the answer. Every one of those extra rows is READABLE — they survive visibleTo —
// so the truncation is the only thing standing between a caller who asked for 1 result and four.

func pageChain(t *testing.T, d *testutil.DB, ws, member string) http.Handler {
	t.Helper()
	store := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := store.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID,
			SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy}, nil
	}
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	h := page.NewHandler(store, d.Pool).
		WithAccess(
			permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore)),
			permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker)),
		).
		WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(),
				"alice@example.com", []authz.Membership{{WorkspaceID: ws, MemberID: member}})))
		})
	})
	r.Route("/v1", func(r chi.Router) { h.Mount(r) })
	return r
}

// seedBulk puts n extra pages in one space with ONE statement. testutil's Page() creates a space
// per call, so seeding 600 through it would create 600 spaces and take minutes.
func seedBulk(t *testing.T, d *testutil.DB, ws, author, spaceID string, n int) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text)
		 SELECT $1,$2,'Runbook '||g,'pg-'||g,$3,'Runbook body '||g FROM generate_series(1,$4) g`,
		spaceID, ws, author, n); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}
}

func rowsAt(t *testing.T, chain http.Handler, path string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", path, rr.Code, rr.Body.String())
	}
	var out []model.Page
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: decode: %v (%s)", path, err, rr.Body.String())
	}
	return len(out)
}

func TestPageList_UpperClamp_BoundsOneRequestsAnswer_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	seed := d.Page(t, ws, alice, "Runbook seed")
	var sp string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id=$1`, seed).Scan(&sp); err != nil {
		t.Fatalf("space of seed page: %v", err)
	}
	seedBulk(t, d, ws, alice, sp, 600) // 601 total — MORE than the clamp, or it cannot be observed
	chain := pageChain(t, d, ws, alice)

	// PREMISE. The clamp is only observable against a corpus larger than it. If the seeding did not
	// take, every assertion below would pass on a short space and prove nothing.
	var total int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pages WHERE space_id=$1`, sp).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 601 {
		t.Fatalf("PREMISE FAILED: %d pages in the space, want 601 — a corpus at or under the "+
			"clamp cannot distinguish a clamp from its absence", total)
	}

	// COUNTERWEIGHT: a clamp that pinned every request to 500 would satisfy the cases below.
	if n := rowsAt(t, chain, "/v1/spaces/"+sp+"/pages?limit=250"); n != 250 {
		t.Fatalf("limit=250 returned %d rows, want 250 — a request under the ceiling must be "+
			"answered as asked", n)
	}
	// EXACTLY AT THE BOUND IS ADMITTED WHOLE.
	if n := rowsAt(t, chain, "/v1/spaces/"+sp+"/pages?limit=500"); n != 500 {
		t.Fatalf("limit=500 returned %d rows, want 500 — the value AT the ceiling must be admitted", n)
	}
	for _, q := range []string{"limit=501", "limit=600", "limit=99999"} {
		if n := rowsAt(t, chain, "/v1/spaces/"+sp+"/pages?"+q); n != 500 {
			t.Fatalf("%s returned %d rows, want 500 — with the store clamp gone this answers with "+
				"the WHOLE space (601 measured), and nothing between the query parameter and the "+
				"SQL bounds it", q, n)
		}
	}
}

func TestPageSearch_TruncationToCallerLimit_KeepsTheOverFetchOutOfTheAnswer_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	seed := d.Page(t, ws, alice, "Runbook seed")
	var sp string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id=$1`, seed).Scan(&sp); err != nil {
		t.Fatalf("space of seed page: %v", err)
	}
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE pages SET content_text='Runbook body seed' WHERE id=$1`, seed); err != nil {
		t.Fatalf("seed text: %v", err)
	}
	seedBulk(t, d, ws, alice, sp, 60) // comfortably more than limit x searchFetchFactor below
	chain := pageChain(t, d, ws, alice)

	// PREMISE. An unbounded-ish read must find far more than the small limits below ask for, or
	// "returned exactly N" is satisfied by a corpus that only had N.
	if n := rowsAt(t, chain, "/v1/workspaces/"+ws+"/pages/search?q=Runbook&limit=61"); n < 40 {
		t.Fatalf("PREMISE FAILED: a wide search returned %d rows; the corpus cannot demonstrate "+
			"truncation at all", n)
	}
	for _, tc := range []struct{ limit, want int }{
		{1, 1}, {3, 3}, {5, 5}, {10, 10},
	} {
		path := "/v1/workspaces/" + ws + "/pages/search?q=Runbook&limit=" + strconv.Itoa(tc.limit)
		if n := rowsAt(t, chain, path); n != tc.want {
			t.Fatalf("limit=%d returned %d rows, want %d — the route OVER-FETCHES "+
				"limit x searchFetchFactor readable rows on purpose, and this truncation is the "+
				"only thing that keeps the surplus out of the answer (measured: with it gone, "+
				"limit=1 returns 4 and limit=3 returns 12)", tc.limit, n, tc.want)
		}
	}
}
