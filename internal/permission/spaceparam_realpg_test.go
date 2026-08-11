package permission_test

// THE `{spaceID}` IN EVERY BY-PAGE ROUTE IS NOT READ BY ANYTHING, AND THIRTEEN CACHE KEYS IN THE
// SPA DEPEND ON THAT BEING TRUE.
//
// `scripts/w31-querykey-census.mjs` reports 13 of 32 useQuery call sites whose queryFn reaches the
// request with a `spaceID` the queryKey does not carry — `["comments", pageID]` calling
// `commentsApi.list(spaceID, pageID)`, and twelve siblings. That was triaged as a smell rather than
// a defect on TWO premises, and this test exists because only one of them was ever measured:
//
//   (b) the SPA cannot produce two different spaceIDs for one pageID — `router/routes.tsx`
//       `PageRoute` reads `spaceID` from `useParams()` and builds `space` as
//       `spaceQ.data ?? {id: spaceID, …}`, so `space.id` IS the route param in both branches. That
//       is a fact about a file and it is checkable by reading it.
//
//   (a) the server does not distinguish the pairs — and THAT is a fact about ~26 routes across 11
//       packages, which a grep can only guess at. `permission.PageResolverFromParam` resolves
//       `{pageID}` and derives the space FROM THE PAGE, so the GATE never reads the URL's space;
//       whether every HANDLER also ignores it is the part a source scan cannot settle, because a
//       handler can reach a param through a helper or the route context.
//
// SO THIS DRIVES IT. Alice owns TWO spaces (creator ⇒ Admin by resolveAccess's owner-is-admin arm)
// and one page, in the first. Every by-page route below is called TWICE — once at the honest
// address, once naming her OTHER space — and the two responses are compared BYTE FOR BYTE.
//
// ⚠⚠ THE FLOOR IS THE POINT OF THE DESIGN, NOT A COURTESY. "the two responses are identical" is
// satisfied by two identical 404s, two identical 500s, or a route that was never mounted and 404'd
// twice — an absence sweep is green on a page that never rendered. So [HONEST-200] asserts that the
// honest address returns 200 for EVERY covered route first, and [COVERAGE] pins the number of
// routes actually driven. Without both, a mounting mistake would report this invariant as held.
//
// ⚠ WHAT THIS IS NOT. It is NOT an authorization test and must not be read as one: alice may open
// both spaces, so no gate is being probed. The tenancy question — bob naming HIS space to reach
// alice's page — is answered by PageResolverFromParam resolving the page's REAL space, and is
// covered elsewhere (a3_guard_test.go, crossresource_realpg_test.go, database/sec4_l2_test.go).
// What is measured here is narrower and is exactly what the 13 keys rest on: **the URL's space is
// not an input to the response.**
//
// ⚠ IF THIS TEST EVER REDS, THE FIX IS PROBABLY NOT HERE. A route that starts depending on
// `{spaceID}` is a legitimate change — but it silently invalidates every one of those 13 queryKeys,
// because two different spaces would then be two different responses cached under one key. Add the
// spaceID to the affected key and to the covered set below, in the same merge.
//
// ⚠ LIMIT, MEASURED AND NOT IMPLIED: the covered set is the by-page GET routes that answer 200 on a
// page with no other state. `/approval` and `/freshness` are NOT here — approval answers only once
// a request exists, and the freshness engine needs three collaborators this file would have to
// construct — and the WRITE routes are not here either, because asserting two writes are identical
// means running each twice and they are not idempotent. Those are the gap; nothing above claims
// otherwise.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/export"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/sharing"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

func TestByPageRoutes_RealPG_TheURLsSpaceIsNotAnInputToTheResponse(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	// TWO spaces, BOTH alice's. The second is the "wrong but authorized" address: she is its
	// creator, so the enforcer would let her through it — which is what makes the comparison a
	// measurement of the HANDLER rather than of the gate.
	home := seedSpaceP(t, d, ws, alice, "Home")
	other := seedSpaceP(t, d, ws, alice, "Other")
	pageID := seedPageP(t, d, ws, home, alice, "Runbook")

	r := mountByPageRoutes(t, d)

	// Every route is written with a `%s` for the space so the two calls differ in EXACTLY that one
	// segment and nothing else.
	routes := []struct{ name, tmpl string }{
		{"page", "/v1/spaces/%s/pages/" + pageID},
		{"versions", "/v1/spaces/%s/pages/" + pageID + "/versions"},
		{"comments", "/v1/spaces/%s/pages/" + pageID + "/comments"},
		{"comment-stats", "/v1/spaces/%s/pages/" + pageID + "/comments/stats"},
		{"changelog", "/v1/spaces/%s/pages/" + pageID + "/changelog/entries"},
		{"share", "/v1/spaces/%s/pages/" + pageID + "/share"},
		{"permissions", "/v1/spaces/%s/pages/" + pageID + "/permissions"},
		{"lock", "/v1/spaces/%s/pages/" + pageID + "/lock"},
		{"analytics", "/v1/spaces/%s/pages/" + pageID + "/analytics"},
		// `?format=markdown` because the route REQUIRES one (400 "format is required" without it),
		// which the [HONEST-200] floor is what discovered. Both calls carry the identical query
		// string, so the two requests still differ in exactly the space segment.
		{"export", "/v1/spaces/%s/pages/" + pageID + "/export?format=markdown"},
	}

	// [COVERAGE] is a hardcoded literal compared against the table's length, not the table's length
	// compared against itself. It fails when a route is dropped from the set without the comment
	// above being updated — the way a covered set quietly shrinks.
	if len(routes) != 10 {
		t.Fatalf("[COVERAGE] the covered set is %d routes, want 10 — a route was added or removed "+
			"without the header's account of what is and is not covered being updated", len(routes))
	}

	get := func(path string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(authz.WithMemberships(req.Context(), "alice@example.com",
			[]authz.Membership{{WorkspaceID: ws, MemberID: alice}}))
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String()
	}

	for _, rt := range routes {
		honestPath := fmtPath(rt.tmpl, home)
		wrongPath := fmtPath(rt.tmpl, other)

		honestCode, honestBody := get(honestPath)
		// [HONEST-200] IS THE FLOOR. Two identical 404s satisfy [SAME-BODY] and prove nothing; this
		// is what makes the comparison below a comparison of two real answers.
		if honestCode != http.StatusOK {
			t.Errorf("[HONEST-200] GET %s (%s, the page's REAL space) = HTTP %d, want 200 — the "+
				"comparison below is only a measurement when both sides are real answers. Body: %s",
				honestPath, rt.name, honestCode, truncate(honestBody))
			continue
		}
		wrongCode, wrongBody := get(wrongPath)
		if wrongCode != honestCode {
			t.Errorf("[SAME-STATUS] %s: naming a DIFFERENT space of the same owner changed the "+
				"status (%d at %s vs %d at %s). The SPA keys this route's cache on pageID alone "+
				"(scripts/w31-querykey-census.mjs), which is only sound while the URL's space is "+
				"not an input.", rt.name, honestCode, honestPath, wrongCode, wrongPath)
			continue
		}
		if wrongBody != honestBody {
			t.Errorf("[SAME-BODY] %s: naming a DIFFERENT space of the same owner changed the "+
				"RESPONSE BODY.\n  %s\n    -> %s\n  %s\n    -> %s\nThirteen useQuery keys in the "+
				"SPA omit spaceID on the reasoning that the server does not read it. One of them "+
				"now caches two different answers under one key.",
				rt.name, honestPath, truncate(honestBody), wrongPath, truncate(wrongBody))
		}
	}
}

// fmtPath fills the ONE %s in a route template. Deliberately not fmt.Sprintf at the call site: the
// point is that the two requests differ in exactly this segment.
func fmtPath(tmpl, spaceID string) string {
	for i := 0; i+1 < len(tmpl); i++ {
		if tmpl[i] == '%' && tmpl[i+1] == 's' {
			return tmpl[:i] + spaceID + tmpl[i+2:]
		}
	}
	panic("route template has no %s for the space: " + tmpl)
}

func truncate(s string) string {
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

// mountByPageRoutes wires the by-page handlers with the REAL enforcers, the same way main.go does.
// It is written here rather than reused from internal/database/sec4_l2_test.go on purpose: that
// chain was built to prove a cross-tenant scope and lending it to this question would lend evidence
// it never gathered. The gateway/authz middleware is not mounted — memberships are injected
// directly — because this test is about the HANDLER's use of a URL segment, and an auth layer in
// front of it would only add a way for the two calls to differ for an unrelated reason.
func mountByPageRoutes(t *testing.T, d *testutil.DB) http.Handler {
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
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		page.NewHandler(pageStore, d.Pool).WithAccess(pageEnf, spaceEnf).Mount(r)
		comment.NewHandler(comment.NewStore(d.Pool)).WithAccess(pageEnf).Mount(r)
		changelog.NewHandler(changelog.NewStore(d.Pool, nil)).WithAccess(pageEnf).Mount(r)
		sharing.NewHandler(sharing.NewStore(d.Pool), nil).WithAccess(pageEnf).Mount(r)
		permission.NewHandler(permStore).WithAccess(spaceEnf, pageEnf).Mount(r)
		pagelock.NewHandler(pagelock.NewStore(d.Pool)).WithAccess(pageEnf).Mount(r)
		analytics.NewHandler(analytics.NewStore(d.Pool)).WithAccess(pageEnf).Mount(r)
		export.NewHandler(export.New(pageStore, spaceStore)).WithAccess(pageEnf).Mount(r)
	})
	return r
}

func seedSpaceP(t *testing.T, d *testutil.DB, wsID, creator, name string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, $2, $3, $4, false) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator,
	).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageP(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text)
		 VALUES ($1, $2, $3, $4, $5, 'the deployment runbook') RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}
