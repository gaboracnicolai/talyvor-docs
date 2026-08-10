package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// THE SECOND COPY OF THE SEAM internal/search WAS HARDENED FOR.
//
// `internal/search`'s privatespace_realpg_test.go records that search "was the one read surface in
// the product that authorized the WORKSPACE and stopped", and that endpoint now runs every row past
// spaceauth (Handler.visibleTo). IT WAS NOT THE ONLY ONE. `page.Handler` registers two more
// workspace-scoped reads at the BOTTOM of Mount, OUTSIDE the /spaces/{spaceID}/pages sub-router and
// therefore outside every `.With(pageEnf.Require(...))` on it:
//
//	r.Get("/workspaces/{wsID}/pages/search", h.Search)
//	r.Get("/workspaces/{wsID}/pages/stale",  h.Stale)
//
// Both call AuthorizeWorkspace and stop there — their own comments say they "mirror
// internal/search/handler.go's guard", and they mirror the WORKSPACE half of it, which is the half
// that was never the defect. Neither consults the permission engine about a space or a page.
//
// ⚠ AND THE PAYLOAD IS STRICTLY LARGER THAN THE ONE ALREADY CLOSED. `internal/search` leaked a
// ts_headline EXCERPT. These two select `columns` — page.scan's full column list — so the response
// is the whole model.Page: `content` (the entire ProseMirror document), `content_text`, the title,
// the space id, the author. The narrower leak was worth a merge; this one returns the document.
//
// Both routes have a live client: frontend/src/api/pages.ts `search()` (useSearchPages, the editor's
// page picker) and `stale()` (the StalePages screen + the sidebar's stale count).
//
// THE FIXTURE IS THIS PACKAGE'S OWN, NOT internal/search's. seedSpace/seedPage there are unexported
// and, more to the point, they seed no `content` — the claim being measured here is about the body
// of the document, so the fixture has to write one. The two lookers are likewise re-derived from
// cmd/docs/main.go rather than borrowed: a helper lends the evidence of the test it was written
// for, not of this one.
//
// The four cases per route are one test on purpose:
//
//  1. bob, no grant            private page MUST NOT appear   ← the defect
//  2. bob, explicit view grant private page MUST appear       ← rules out `AND sp.private = false`
//  3. alice, the space creator private page MUST appear       ← resolveAccess's owner-is-admin arm
//  4. every case                public page MUST appear        ← a fix that empties the endpoint fails
//
// so a filter that simply drops private spaces, or one that drops everything, fails here rather
// than passing quietly.
func TestPageSearchAndStale_PrivateSpace_NotVisibleWithoutGrant_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	// A PUBLIC space + page every member may read. This is the positive control INSIDE the sample:
	// it proves the query matched and the endpoint served, so "no rows" can never be misread as
	// "the private page was filtered".
	publicSpace := seedSpaceP(t, d, ws, alice, "Public Handbook", false)
	publicPage := seedPageP(t, d, ws, publicSpace, alice, "Onboarding Handbook",
		"the quarterly onboarding handbook everyone reads",
		`{"type":"doc","content":[{"type":"paragraph","text":"public onboarding body"}]}`)

	// A PRIVATE space created by alice. bob is a workspace member with NO grant, so
	// permission.resolveAccess gives him AccessNone on it.
	privSpace := seedSpaceP(t, d, ws, alice, "Board Private", true)
	privPage := seedPageP(t, d, ws, privSpace, alice, "Quarterly Board Memo",
		"quarterly revenue missed plan and we will cut the SECRETLAYOFF programme",
		`{"type":"doc","content":[{"type":"paragraph","text":"SECRETLAYOFF: 240 roles, board only"}]}`)

	// Make BOTH pages stale so /pages/stale returns them: stale_after_days > 0, updated_at older
	// than that window, never verified. Backdating is the only way to reach the predicate.
	//
	// THE TWO DATES DIFFER ON PURPOSE. Store.Search is `ORDER BY updated_at DESC`, so the PRIVATE
	// memo sorts ABOVE the public handbook — which is what makes the limit=1 case below sharp: a
	// filter applied AFTER the SQL LIMIT returns the denied caller an EMPTY page while a document
	// they may read was waiting directly behind the one they may not.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET stale_after_days = 30, last_verified_at = NULL,
		        updated_at = CASE WHEN id = $1 THEN NOW() - INTERVAL '100 days'
		                                       ELSE NOW() - INTERVAL '400 days' END
		  WHERE id = ANY($2)`, privPage, []string{publicPage, privPage}); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	store := page.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)

	// The two lookers and the two enforcers are cmd/docs/main.go's, line for line (main.go:378-403),
	// so the handler under test is mounted with the SAME access wiring production gives it. Neither
	// enforcer reaches the two routes being measured — that is the finding — and wiring them anyway
	// is what makes "the test skipped WithAccess" unavailable as an explanation.
	spaceStore := space.NewStore(d.Pool)
	spaceLooker := func(ctx context.Context, id string) (permission.SpaceMeta, error) {
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.SpaceMeta{}, err
		}
		return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
	}
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := store.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID,
			SpaceID:     pg.SpaceID, SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID", spaceLooker))
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))

	// get drives the REAL route through the REAL handler, wired as cmd/docs/main.go wires it.
	get := func(email, memberID, path string) []model.Page {
		t.Helper()
		h := page.NewHandler(store, d.Pool).
			WithAccess(pageEnf, spaceEnf).
			WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))

		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })

		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s as %s = %d, want 200: %s", path, email, rr.Code, rr.Body.String())
		}
		var out []model.Page
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v (%s)", path, err, rr.Body.String())
		}
		return out
	}

	searchPath := "/v1/workspaces/" + ws + "/pages/search?q=quarterly&limit=50"
	stalePath := "/v1/workspaces/" + ws + "/pages/stale"

	has := func(rs []model.Page, id string) bool {
		for _, r := range rs {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	// leaked returns the OFFENDING ROW AS SERVED, so the failure message is the measurement
	// rather than a claim about it.
	leaked := func(rs []model.Page) string {
		for _, r := range rs {
			if r.ID == privPage {
				b, _ := json.Marshal(r)
				return string(b)
			}
		}
		return ""
	}

	for _, tc := range []struct {
		name, path string
	}{
		{"pages/search", searchPath},
		{"pages/stale", stalePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// ── 1. bob, no grant. Premise first: he must see the PUBLIC page, or an absent
			// private page proves nothing (the query matched nothing / the route served nothing).
			bobRows := get("bob@example.com", bob, tc.path)
			if !has(bobRows, publicPage) {
				t.Fatalf("[A-PREMISE/%s] PREMISE FAILED: bob cannot see the PUBLIC page either — the "+
					"endpoint returned nothing to filter, so an absent private page would prove "+
					"nothing. rows=%d", tc.name, len(bobRows))
			}
			if row := leaked(bobRows); row != "" {
				// Name the body explicitly: the severity claim is that `content` is in the payload,
				// not merely a title.
				body := ""
				if strings.Contains(row, "SECRETLAYOFF: 240 roles") {
					body = " — AND THE ROW CARRIES THE FULL `content`, not an excerpt"
				}
				t.Errorf("[A-LEAK/%s] LEAK: bob has NO grant on the private space and read its page%s: %s",
					tc.name, body, row)
			}

			// ── 4. THE HIDDEN ROW MUST NOT CONSUME A SLOT. Only /pages/search takes a limit;
			// /pages/stale has none, so there is no truncation there to race. The private memo
			// sorts above the public handbook, so with limit=1 a filter that runs after the SQL
			// LIMIT hands denied bob an empty list.
			if tc.name == "pages/search" {
				one := get("bob@example.com", bob,
					"/v1/workspaces/"+ws+"/pages/search?q=quarterly&limit=1")
				if len(one) != 1 || one[0].ID != publicPage {
					t.Errorf("[A-SLOT/search] limit=1 as denied bob returned %d rows, want exactly the public page — "+
						"a row he cannot read is eating his result slot (the filter must run BEFORE "+
						"the truncation, over an over-fetched window)", len(one))
				}
			}

			// ── 3. alice created the space: resolveAccess's owner-is-admin arm must keep it visible.
			aliceRows := get("alice@example.com", alice, tc.path)
			if !has(aliceRows, privPage) {
				t.Errorf("[A-OWNER/%s] OVER-CORRECTION: the space's CREATOR cannot see her own private "+
					"page (rows=%d) — owner-is-admin lost", tc.name, len(aliceRows))
			}
			// ⚠ THERE WAS A FOURTH ASSERTION HERE — "alice must still see the PUBLIC page" — AND
			// IT WAS DELETED RATHER THAN DECORATED. The control harness reported it claimed by
			// no control, and the reason is structural, not a gap in the controls: alice CREATED
			// both spaces, so permission.resolveAccess's owner-is-admin arm keeps the public page
			// visible to her under every one-line mutation that does not empty the endpoint
			// outright — and a mutation that DOES empty it trips the PREMISE t.Fatalf above,
			// which aborts this subtest before this line is ever evaluated. It could therefore
			// only ever agree with an assertion that had already spoken. An invariant held twice
			// cannot be breached by any single control, so the copy goes rather than the control.
		})
	}

	// ── 2. bob WITH an explicit view grant on the private space: the page must come back on BOTH
	// routes. This is the assertion a blanket `AND sp.private = false` fails. Granted last so cases
	// 1 and 3 run while he is still denied.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`,
		privSpace, bob, ws); err != nil {
		t.Fatalf("grant: %v", err)
	}
	for _, tc := range []struct {
		name, path string
	}{
		{"pages/search", searchPath},
		{"pages/stale", stalePath},
	} {
		granted := get("bob@example.com", bob, tc.path)
		if !has(granted, privPage) {
			t.Errorf("[A-GRANT/%s] OVER-CORRECTION: bob holds an explicit 'view' grant on the private "+
				"space and still cannot see its page (rows=%d) — the filter is not consulting "+
				"resolveAccess", tc.name, len(granted))
		}
		if !has(granted, publicPage) {
			t.Errorf("[A-GRANTPUB/%s] granted bob lost the public page (rows=%d)", tc.name, len(granted))
		}
	}
}

func seedSpaceP(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator, private,
	).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

// seedPageP writes `content` as well as `content_text` — internal/search's seeder writes only the
// latter, and the size of THIS leak is the point.
func seedPageP(t *testing.T, d *testutil.DB, wsID, spaceID, author, title, body, content string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, body, content,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}
