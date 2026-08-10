package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// SEARCH WAS THE ONE READ SURFACE THAT NEVER ASKED THE PERMISSION ENGINE.
//
// Every other read of a page's content goes through permission.RequireAccess (a chi middleware
// keyed on a URL param) or spaceauth (for a body-named target). Search has neither: ITS TARGET IS
// A QUERY, NOT AN ID, so no resolver could reach it — and the handler authorized the WORKSPACE and
// stopped there. page.SearchWithRank's SQL filters on `workspace_id`, `is_template` and an optional
// space, and nothing else.
//
// The product's own rule (permission.resolveAccess) is that a PRIVATE space gives a workspace
// member NOTHING without an explicit grant. Measured through the real endpoint against a real
// Postgres at 69bd2d9, a member denied on a private space received the page title, the space name,
// and a ts_headline EXCERPT OF THE BODY with the matched terms wrapped in <mark>:
//
//	{"page_id":"8852…","page_title":"Quarterly Board Memo","space_name":"Board Private",
//	 "headline":"<mark>quarterly</mark> revenue missed plan and we will cut the SECRETLAYOFF programme", …}
//
// ⚠ RED-FIRST IS PARTIAL AND STATED. On the unmodified tree cases 1 and 4 FAILED; cases 2 and 3
// PASSED — necessarily, because with no filter at all everything is visible. 2 and 3 are the
// over-correction refusals, earned by controls C2/C3 rather than by a red.
//
// The four cases are one test on purpose:
//
//  1. bob, no grant            private page MUST NOT appear  ← the defect
//  2. bob, explicit view grant private page MUST appear      ← rules out `AND sp.private = false`
//  3. alice, the space creator private page MUST appear      ← resolveAccess's owner-is-admin arm
//  4. bob, no grant, limit=1   gets the PUBLIC page          ← the hidden row must not eat his slot
//
// and in every case the PUBLIC page must come back, so a fix that empties the endpoint fails here
// rather than passing quietly.
func TestSearch_PrivateSpace_NotVisibleWithoutGrant_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	// A PUBLIC space + page every member may read. This is the positive control inside the
	// sample: it proves the query matched and the endpoint served, so a "no rows" answer can
	// never be read as "the private page was filtered".
	publicSpace := seedSpace(t, d, ws, alice, "Public Handbook", false)
	publicPage := seedPage(t, d, ws, publicSpace, alice, "Onboarding Handbook",
		"the quarterly onboarding handbook everyone reads")

	// A PRIVATE space created by alice. bob is a workspace member with NO grant on it, so
	// resolveAccess gives him AccessNone. Its body is written to out-rank the public page on
	// this query (two hits, and one of them in the title), which is what makes case 4 sharp.
	privSpace := seedSpace(t, d, ws, alice, "Board Private", true)
	privPage := seedPage(t, d, ws, privSpace, alice, "Quarterly Board Memo",
		"quarterly revenue missed plan and we will cut the SECRETLAYOFF programme")

	store := page.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)
	sem := newSemanticSearch(lensintegration.New("", ""), nil)

	searchN := func(email, memberID string, limit int) []Result {
		t.Helper()
		// Wired exactly as cmd/docs/main.go wires it: the shipped spaceauth authorizer over the
		// real permission store and the real page+space meta join.
		h := NewHandler(store, sem).WithAccess(
			spaceauth.New(space.NewStore(d.Pool), permStore).WithPageMeta(pageMetaLooker(d)))

		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })

		req := httptest.NewRequest(http.MethodGet,
			"/v1/workspaces/"+ws+"/search?q=quarterly&type=fulltext&limit="+strconv.Itoa(limit), nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("search as %s = %d, want 200: %s", email, rr.Code, rr.Body.String())
		}
		var body response
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, rr.Body.String())
		}
		return body.Results
	}
	search := func(email, memberID string) []Result { return searchN(email, memberID, 50) }

	has := func(rs []Result, pageID string) bool {
		for _, r := range rs {
			if r.PageID == pageID {
				return true
			}
		}
		return false
	}
	leaked := func(rs []Result) string {
		for _, r := range rs {
			if r.PageID == privPage {
				b, _ := json.Marshal(r)
				return string(b)
			}
		}
		return ""
	}

	// ── 1. bob, no grant: the private page must not be in his results at all.
	bobRows := search("bob@example.com", bob)
	if !has(bobRows, publicPage) {
		t.Fatalf("PREMISE FAILED: bob cannot see the PUBLIC page either — the query matched nothing, "+
			"so an absent private page would prove nothing. rows=%d", len(bobRows))
	}
	if row := leaked(bobRows); row != "" {
		t.Errorf("LEAK: bob has no grant on the private space and read its page through search: %s", row)
	}

	// ── 4. THE HIDDEN ROW MUST NOT CONSUME A SLOT. The private memo out-ranks the handbook, so
	// with limit=1 a filter applied AFTER the LIMIT returns bob an EMPTY page while a document he
	// may read was waiting behind it. Asserted before the grant lands, while he is still denied.
	if one := searchN("bob@example.com", bob, 1); len(one) != 1 || one[0].PageID != publicPage {
		t.Errorf("limit=1 as denied bob returned %d rows (%+v), want exactly the public page — a row "+
			"he cannot read is eating his result slot (filter must run BEFORE the truncation)", len(one), one)
	}

	// ── 2. bob WITH an explicit view grant: the private page must come back. This is the
	// assertion a blanket private-space filter fails.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`,
		privSpace, bob, ws); err != nil {
		t.Fatalf("grant: %v", err)
	}
	granted := search("bob@example.com", bob)
	if !has(granted, privPage) {
		t.Errorf("OVER-CORRECTION: bob holds an explicit 'view' grant on the private space and still "+
			"cannot find its page (rows=%d) — the filter is not consulting resolveAccess", len(granted))
	}
	if !has(granted, publicPage) {
		t.Errorf("granted bob lost the public page too (rows=%d)", len(granted))
	}

	// ── 3. alice created the space: resolveAccess's first arm makes her admin regardless of any
	// grant row, so she must see it.
	aliceRows := search("alice@example.com", alice)
	if !has(aliceRows, privPage) {
		t.Errorf("the space's CREATOR cannot find her own private page (rows=%d) — owner-is-admin lost", len(aliceRows))
	}
	if !has(aliceRows, publicPage) {
		t.Errorf("alice lost the public page (rows=%d)", len(aliceRows))
	}
}

// pageMetaLooker builds the page+space meta the permission engine needs — the same join
// cmd/docs/main.go's pageLooker performs.
func pageMetaLooker(d *testutil.DB) func(ctx context.Context, pageID string) (permission.PageMeta, error) {
	return func(ctx context.Context, pageID string) (permission.PageMeta, error) {
		var md permission.PageMeta
		err := d.Pool.QueryRow(ctx,
			`SELECT p.workspace_id, p.space_id, sp.created_by, sp.private, p.created_by
			   FROM pages p JOIN spaces sp ON sp.id = p.space_id
			  WHERE p.id = $1`, pageID,
		).Scan(&md.WorkspaceID, &md.SpaceID, &md.SpaceCreatedBy, &md.SpacePrivate, &md.PageCreatedBy)
		return md, err
	}
}

func seedSpace(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
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

func seedPage(t *testing.T, d *testutil.DB, wsID, spaceID, author, title, body string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, body,
	).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}
