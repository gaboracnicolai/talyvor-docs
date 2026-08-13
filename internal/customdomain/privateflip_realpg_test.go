package customdomain_test

// A SPACE MADE PRIVATE STAYED WORLD-READABLE AT ITS CUSTOM DOMAIN, AND THE GUARD THAT WAS MEANT TO
// STOP THAT RAN ONCE, MONTHS EARLIER.
//
// `Store.Create` refuses to MAP a private space, and says why in its own words: "a custom domain
// publishes every page in the space it maps to, and Docs has no per-page publish control yet, so
// mapping a private space would expose all of it". That check is correct and it is untouched here.
// It is also a statement about ONE INSTANT — the moment of mapping.
//
// `private` is not frozen at that instant. It is on `space/store.go`'s `updatable` allow-list, so
// `PATCH /v1/spaces/{spaceID} {"private":true}` is a shipped route, reachable by any space admin,
// and it is the EXACT action a user takes when they mean "stop showing this to the world".
//
// ⚠⚠ MEASURED ON REAL POSTGRES BEFORE THE FIX, through the shipped PATCH and the shipped
// DomainRouter, unauthenticated on the public side:
//
//	map docs.example.com -> public space          allowed (correctly)
//	PATCH /v1/spaces/{s} {"private":true}         200, spaces.private = true
//	GET http://docs.example.com/                  200 — listing every page TITLE in the space
//	GET http://docs.example.com/salary-bands      200 — serving the page BODY
//
// The read path consulted no privacy predicate anywhere: publicIndex lists whatever ListBySpace
// returns, and publicPage serves any slug via GetBySlug. Both were reached with no Authorization
// header at all — this is not a within-tenant tier leak like the other copies of the seam, it is
// the open internet.
//
// ⚠ THE FIX IS THE READ-TIME HALF OF THE RULE CREATE ALREADY WRITES DOWN, and nothing more. Two
// alternatives were NOT taken because they are product decisions rather than an invariant:
// unmapping the domain when a space is made private (destroys configuration the admin may want
// back), and refusing the PATCH while a domain is mapped (blocks a privacy action on the grounds
// that publishing is configured, which is backwards). Serving 404 leaves both open.
//
//	[PRE-PUBLIC]     while public, the domain serves the index and the page   ← the vacuity floor
//	[FLIP-INDEX]     once private, the index must not name the pages
//	[FLIP-PAGE]      once private, the page body must not be served
//	[FLIP-SEARCH]    once private, the stub search route is 404 too
//	[FLIP-STATUS]    the refusal is 404, not a 200 saying "not published"
//	[SHIPPED-ROUTE]  the flip is reachable through PATCH /v1/spaces/{spaceID}
//	[CREATE-HELD]    mapping a private space is STILL refused                 ← the old guard lives
//	[RESTORED]       made public again, the domain serves again               ← not a one-way kill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/customdomain"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const (
	flipDomain = "docs.flip-example.com"
	flipTitle  = "Salary Bands"
	flipBody   = "BAND-4-IS-180K"
)

// flipPages is cmd/docs/main.go's publicPageAdapter, re-derived rather than imported: the public
// renderer is only as scoped as the adapter main.go hands it, and borrowing a helper borrows its
// evidence.
type flipPages struct{ pages *page.Store }

func (a *flipPages) GetByID(ctx context.Context, id string) (*model.Page, error) {
	return a.pages.GetByID(ctx, id)
}
func (a *flipPages) GetBySlug(ctx context.Context, spaceID, slug string) (*model.Page, error) {
	return a.pages.GetBySlug(ctx, spaceID, slug)
}
func (a *flipPages) ListBySpace(ctx context.Context, spaceID string) ([]model.Page, error) {
	return a.pages.List(ctx, page.PageFilter{SpaceID: spaceID, Limit: 500})
}

func TestCustomDomain_SpaceMadePrivateStopsBeingServed_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice-flip@example.com")

	pageStore := page.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)
	cdStore := customdomain.NewStore(d.Pool)

	sp, err := spaceStore.Create(ctx, model.Space{
		WorkspaceID: ws, Name: "Handbook", Slug: "sp-flip", CreatedBy: alice, Private: false,
	})
	if err != nil {
		t.Fatalf("create space: %v", err)
	}
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content, content_text)
		 VALUES ($1,$2,$3,'salary-bands',$4,'{"type":"doc"}',$5)`,
		sp.ID, ws, flipTitle, alice, flipBody); err != nil {
		t.Fatalf("seed page: %v", err)
	}

	spaceID := sp.ID
	cd, err := cdStore.Create(ctx, ws, flipDomain, alice, &spaceID)
	if err != nil {
		t.Fatalf("mapping a PUBLIC space must be allowed: %v", err)
	}
	// verified is what makes DomainRouter route to the public handler at all. The DNS half is a
	// different subject; set it exactly as a successful Verify() does.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE custom_domains SET verified = true, ssl_status='active' WHERE id = $1`, cd.ID); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	router := customdomain.DomainRouter(cdStore,
		customdomain.NewHandler(cdStore, &flipPages{pages: pageStore}).PublicHandler(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// If a request ever falls through to the main router on a custom host, that is a
			// different bug and this sentinel makes it visible instead of looking like a refusal.
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte("FELL-THROUGH-TO-MAIN"))
		}), "0.0.0.0")

	// The public side carries NO Authorization and no membership context. That is the point.
	hit := func(path string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "http://"+flipDomain+path, nil)
		req.Host = flipDomain
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		body := rr.Body.String()
		if strings.Contains(body, "FELL-THROUGH-TO-MAIN") {
			t.Fatalf("GET %s reached the MAIN router on a verified custom host — the refusals "+
				"below would be measuring the wrong handler", path)
		}
		return rr.Code, body
	}

	// ── [PRE-PUBLIC] the vacuity floor. Everything below is an absence assertion, and an absence
	// is free if the domain never served anything in the first place.
	if code, body := hit("/"); code != http.StatusOK || !strings.Contains(body, flipTitle) {
		t.Fatalf("[PRE-PUBLIC] the index does not serve the page while the space is PUBLIC "+
			"(%d, title present=%v) — every assertion below would pass on a dead domain",
			code, strings.Contains(body, flipTitle))
	}
	if code, body := hit("/salary-bands"); code != http.StatusOK || !strings.Contains(body, flipBody) {
		t.Fatalf("[PRE-PUBLIC] the page does not serve its body while the space is PUBLIC "+
			"(%d, body present=%v)", code, strings.Contains(body, flipBody))
	}

	// ── the flip, through the SHIPPED route, as a space admin. Not raw SQL: the finding is that a
	// user action available in the product produces this state.
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID",
		func(ctx context.Context, id string) (permission.SpaceMeta, error) {
			s, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
			if err != nil {
				return permission.SpaceMeta{}, err
			}
			return permission.SpaceMeta{WorkspaceID: s.WorkspaceID, Private: s.Private, CreatedBy: s.CreatedBy}, nil
		}))
	admin := chi.NewRouter()
	admin.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), "alice-flip@example.com",
				[]authz.Membership{{WorkspaceID: ws, MemberID: alice}})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	admin.Route("/v1", func(r chi.Router) { space.NewHandler(spaceStore).WithAccess(spaceEnf).Mount(r) })
	patch := func(payload string) int {
		req := httptest.NewRequest(http.MethodPatch, "/v1/spaces/"+spaceID, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		admin.ServeHTTP(rr, req)
		return rr.Code
	}

	if code := patch(`{"private":true}`); code != http.StatusOK {
		t.Fatalf("[SHIPPED-ROUTE] PATCH {\"private\":true} = %d, want 200 — if this route no "+
			"longer flips privacy, the finding's premise is gone and this file must be re-measured "+
			"rather than left asserting a state nothing can reach", code)
	}
	var private bool
	if err := d.Pool.QueryRow(ctx, `SELECT private FROM spaces WHERE id = $1`, spaceID).Scan(&private); err != nil {
		t.Fatalf("read privacy: %v", err)
	}
	if !private {
		t.Fatalf("[SHIPPED-ROUTE] the PATCH returned 200 and spaces.private is still false — the " +
			"column the public renderer now reads is not the column the route writes")
	}

	// ── the finding.
	code, body := hit("/")
	if strings.Contains(body, flipTitle) {
		t.Errorf("[FLIP-INDEX] the public index still names %q after the space was made private. "+
			"The mapping-time check governs the mapping; nothing governed the response", flipTitle)
	}
	if code != http.StatusNotFound {
		t.Errorf("[FLIP-STATUS] the index answered %d for an unpublished space, want 404 — a 200 "+
			"page saying 'not published' is a page a crawler keeps and a monitor calls healthy", code)
	}
	code, body = hit("/salary-bands")
	if strings.Contains(body, flipBody) {
		t.Errorf("[FLIP-PAGE] the public renderer still served the page BODY of a private space, "+
			"unauthenticated, at %s", flipDomain)
	}
	if code != http.StatusNotFound {
		t.Errorf("[FLIP-STATUS] the page answered %d for an unpublished space, want 404", code)
	}
	if code, _ := hit("/search?q=band"); code != http.StatusNotFound {
		t.Errorf("[FLIP-SEARCH] the search route answered %d on an unpublished domain, want 404. "+
			"It returns no page data today, but a live route on a domain that is meant to be gone "+
			"is a liveness signal, and the next implementation of this stub inherits the gate", code)
	}

	// ── [CREATE-HELD] the guard that already existed must still exist. This fix is additive.
	if _, err := cdStore.Create(ctx, ws, "second.flip-example.com", alice, &spaceID); err == nil {
		t.Errorf("[CREATE-HELD] mapping a PRIVATE space was allowed — the create-time refusal is " +
			"gone. The read-time gate does not replace it: without create-time refusal an admin " +
			"gets a domain that silently serves nothing until they notice")
	}

	// ── [RESTORED] making the space public again restores service. The gate is a live read of the
	// current state, not a one-way kill switch on the domain.
	if code := patch(`{"private":false}`); code != http.StatusOK {
		t.Fatalf("[RESTORED] PATCH {\"private\":false} = %d, want 200", code)
	}
	if code, body := hit("/salary-bands"); code != http.StatusOK || !strings.Contains(body, flipBody) {
		t.Errorf("[RESTORED] the domain did not resume serving after the space was made public "+
			"again (%d, body present=%v) — the gate must read the current state, not latch",
			code, strings.Contains(body, flipBody))
	}
}
