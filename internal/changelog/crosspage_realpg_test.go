package changelog_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/changelog"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// AN ENTRY ID IS ANSWERED BY WHATEVER PAGE THE CALLER NAMES, NOT BY THE PAGE THE ENTRY IS ON.
//
// The four by-id changelog routes are gated on {pageID}:
//
//	GET    /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}          View
//	PATCH  /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}          Edit
//	DELETE /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}          Edit
//	POST   /spaces/{spaceID}/pages/{pageID}/changelog/entries/{id}/publish  Edit
//
// pageEnf resolves {pageID} and answers about THAT page. The handler then passes only {id} to the
// store, which scopes by `workspace_id = ANY($2)`. So the pair is never checked for consistency:
// the gate authorizes one object and the statement acts on another, and the only thing standing
// between them is the WORKSPACE — the ring both objects are already inside.
//
// ⚠ MEASURED THROUGH THE REAL ROUTES ON REAL POSTGRES BEFORE ANY CHANGE, bob a workspace member
// with NO grant on alice's private space, one entry on a page in that private space:
//
//	bob GET  it at ITS OWN address  (/spaces/{priv}/pages/{privPage}/…)  -> 403 forbidden
//	bob GET  it via a PUBLIC page   (/spaces/{pub}/pages/{pubPage}/…)    -> 200, the whole row,
//	                                        content = "SECRETLAYOFF: the full internal note…"
//	bob PATCH   it via HIS OWN page -> 200, title and summary rewritten
//	bob PUBLISH it via HIS OWN page -> 200, published_at set — a private page's release note is
//	                                        now in the workspace RSS feed
//	bob DELETE  it via HIS OWN page -> 200, and the row count went to 0
//
// The SAME entry, refused at 403 through its own address and served at 200 through a borrowed
// one. The gate is not broken; it is answering about a different object than the statement.
//
// ⚠ ONE MEMBER, NO GRANTS, NO ADMIN: reading needs only AccessView, which resolveAccess hands
// every workspace member on any PUBLIC space. The three writes need AccessEdit, and `POST /spaces`
// is registered with no enforcer at all — so a member creates a space, is its creator, and
// resolveAccess's owner-is-admin arm gives them Edit on their own page. There is no privileged
// step in the chain.
//
// ⚠ WHY THE CLASS GUARD IS GREEN OVER IT, AND THIS IS THE PART WORTH KEEPING: .semgrep/
// operate-by-id-tenancy.yml exists for exactly this shape and its predicate is
// `workspace_id = ANY`. All four statements have it. A rule written for the outer ring cannot
// see the inner one, by construction — it is satisfied by the very predicate that is the defect.
//
// ⚠ AND THE REPOSITORY ALREADY HOLDS THE RIGHT SHAPE TWICE, which is what makes this an omission
// rather than a design: sharing.Store.Revoke is `DELETE … WHERE id = $1 AND page_id = $2` with a
// comment saying it ties {id} to "the page the AccessAdmin gate verified", and comment.Store's
// *InWorkspaces methods run assertInPage(commentID, pageID, wsIDs) first. Changelog is the
// outlier, not the precedent.
//
// ⚠ NO CLIENT LOSES ANYTHING, MEASURED NOT ASSUMED: every SPA call goes through
// useChangelog(spaceID, pageID), and the ids it holds come from changelogApi.list(spaceID,
// pageID) — which is page-scoped already. A mismatched pair is not something the product asks
// for; it is only something it answers.
//
// THE FIXTURE IS THIS PACKAGE'S OWN. It seeds `content` and `summary` because the size of the
// leak is the claim, and it reads every row assertion with SQL against the pool rather than
// through GetEntry — an oracle that shares a code path with its subject is not an oracle.
func TestChangelogByIDRoutes_MustBelongToTheAuthorizedPage_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	// A PUBLIC space alice owns. Every workspace member resolves to AccessView on it with no
	// grant at all — this is the address bob borrows to read.
	pubSpace := seedSpaceX(t, d, ws, alice, "Public Handbook", false)
	pubPage := seedPageX(t, d, ws, pubSpace, alice, "Onboarding")
	// A PRIVATE space alice owns. bob has no grant, so resolveAccess gives him AccessNone.
	privSpace := seedSpaceX(t, d, ws, alice, "Board Private", true)
	privPage := seedPageX(t, d, ws, privSpace, alice, "Board Memo")
	// bob's OWN space. He created it, so owner-is-admin gives him Edit — this is the address he
	// borrows to write. Nobody granted him anything.
	bobSpace := seedSpaceX(t, d, ws, bob, "Bobs Own", false)
	bobPage := seedPageX(t, d, ws, bobSpace, bob, "Bobs Page")

	secret := seedEntryX(t, d, ws, privPage, alice, "v9.9.9", "Layoff programme",
		"240 roles cut, board only", "SECRETLAYOFF: the full internal note, board only")
	// A SECOND private entry, targeted by nothing but the grant case at the end, so a failed
	// delete assertion cannot cascade into the grant assertion and report two findings for one.
	secret2 := seedEntryX(t, d, ws, privPage, alice, "v9.9.8", "Layoff programme, part two",
		"the rest of it", "SECRETLAYOFF2: the second internal note")
	pubEntry := seedEntryX(t, d, ws, pubPage, alice, "v1.0.0", "Handbook refresh",
		"public summary", "public body")
	bobEntry := seedEntryX(t, d, ws, bobPage, bob, "v2.0.0", "Bobs release",
		"bobs summary", "bobs body")

	store := changelog.NewStore(d.Pool, nil)
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)

	// The looker and the enforcer are cmd/docs/main.go's, re-derived here rather than borrowed —
	// a helper lends the evidence of the test it was written for. Wiring the REAL enforcer is
	// what makes "the test skipped WithAccess" unavailable as an explanation for anything below.
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

	do := func(email, memberID, method, path, body string) (int, string) {
		t.Helper()
		h := changelog.NewHandler(store).WithAccess(pageEnf)
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c := authz.WithMemberships(req.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, req.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	entryURL := func(spaceID, pageID, suffix string) string {
		return "/v1/spaces/" + spaceID + "/pages/" + pageID + "/changelog/entries" + suffix
	}

	t.Run("read", func(t *testing.T) {
		// ── PREMISE. An instrument check, and it is UNCLAIMED BY CONSTRUCTION: no mutation to
		// the page scope can make bob's own page stop answering without also moving X-OWN-*.
		// If this fails, every absence below is an absence of a working route.
		if code, body := do("bob@example.com", bob, http.MethodGet, entryURL(pubSpace, pubPage, "/"+pubEntry), ""); code != http.StatusOK {
			t.Fatalf("[X-PREMISE] PREMISE FAILED: bob cannot read a PUBLIC page's own entry through "+
				"its own address (%d %s) — the route is not serving, so a refusal below would prove nothing",
				code, body)
		}

		// ── X-HONEST. The gate itself, at the entry's OWN address. This is what makes the next
		// assertion a statement about the PAIR rather than about bob's access: the product
		// already knows he may not have this row.
		if code, _ := do("bob@example.com", bob, http.MethodGet, entryURL(privSpace, privPage, "/"+secret), ""); code == http.StatusOK {
			t.Errorf("[X-HONEST] bob read the private page's entry at its OWN address (200) — the " +
				"page enforcer is not refusing, so the leak below is a broken gate rather than a " +
				"mismatched pair, and the fix belongs somewhere else")
		}

		// ── X-LEAK-GET. THE DEFECT.
		code, body := do("bob@example.com", bob, http.MethodGet, entryURL(pubSpace, pubPage, "/"+secret), "")
		if code != http.StatusNotFound {
			if code == http.StatusOK {
				t.Errorf("[X-LEAK-GET] LEAK: bob has NO grant on the private space and read its "+
					"changelog entry by naming a PUBLIC page in the URL: %s", body)
			} else {
				t.Errorf("[X-LEAK-GET] a mismatched (page, entry) pair answered %d, want 404 — 404 is "+
					"the no-oracle answer (403 would confirm the id exists somewhere the caller "+
					"cannot reach): %s", code, body)
			}
		}

		// ── X-LIST. The page-scoped sibling, in the same fixture and the same request path: it
		// must show pubPage's entry and NOT the private one. Without it, "the entry did not come
		// back" would be satisfied by a route that returns nothing at all.
		code, body = do("bob@example.com", bob, http.MethodGet, entryURL(pubSpace, pubPage, ""), "")
		if code != http.StatusOK || !strings.Contains(body, pubEntry) {
			t.Errorf("[X-LIST] the page-scoped list did not serve the public page's own entry "+
				"(%d %s) — the fixture, not the scope, would be answering", code, body)
		} else if strings.Contains(body, secret) {
			t.Errorf("[X-LIST] the page-scoped list returned the PRIVATE page's entry: %s", body)
		}
	})

	t.Run("update", func(t *testing.T) {
		// ── X-OWN-PATCH. The over-correction direction: the honest pair must keep working.
		if code, body := do("bob@example.com", bob, http.MethodPatch, entryURL(bobSpace, bobPage, "/"+bobEntry),
			`{"summary":"bob edited his own release note"}`); code != http.StatusOK {
			t.Errorf("[X-OWN-PATCH] OVER-CORRECTION: bob cannot edit an entry on HIS OWN page (%d %s)",
				code, body)
		} else if got := summaryOfX(t, d, bobEntry); got != "bob edited his own release note" {
			t.Errorf("[X-OWN-PATCH] the route answered 200 and the row did not move: summary=%q", got)
		}

		// ── X-LEAK-PATCH. Asserted on the ROW, read with SQL: a 4xx with the row already
		// rewritten would satisfy a status-only assertion.
		before := summaryOfX(t, d, secret)
		code, body := do("bob@example.com", bob, http.MethodPatch, entryURL(bobSpace, bobPage, "/"+secret),
			`{"title":"OWNED BY BOB","summary":"bob rewrote alices private release note"}`)
		if after := summaryOfX(t, d, secret); after != before {
			t.Errorf("[X-LEAK-PATCH] LEAK: bob rewrote the private page's changelog entry by naming "+
				"HIS OWN page in the URL (%d %s) — summary %q -> %q", code, body, before, after)
		}
	})

	t.Run("publish", func(t *testing.T) {
		// ── X-OWN-PUBLISH. Over-correction direction, asserted on the column publish exists to set.
		if code, body := do("bob@example.com", bob, http.MethodPost, entryURL(bobSpace, bobPage, "/"+bobEntry+"/publish"), ""); code != http.StatusOK {
			t.Errorf("[X-OWN-PUBLISH] OVER-CORRECTION: bob cannot publish an entry on HIS OWN page (%d %s)",
				code, body)
		} else if !publishedX(t, d, bobEntry) {
			t.Errorf("[X-OWN-PUBLISH] the route answered 200 and published_at is still NULL")
		}

		// ── X-LEAK-PUBLISH. The one that ESCAPES the space: GetPublicFeed selects every entry
		// with published_at IS NOT NULL in the caller's workspaces, so publishing a private
		// page's note puts its version, title and summary into every member's changelog feed.
		code, body := do("bob@example.com", bob, http.MethodPost, entryURL(bobSpace, bobPage, "/"+secret+"/publish"), "")
		if publishedX(t, d, secret) {
			t.Errorf("[X-LEAK-PUBLISH] LEAK: bob PUBLISHED the private page's changelog entry by "+
				"naming HIS OWN page in the URL (%d %s) — published_at is set, so it is now in the "+
				"workspace-wide RSS feed", code, body)
		}
	})

	t.Run("delete", func(t *testing.T) {
		// ── X-LEAK-DELETE. Destructive, and it runs before the own-page delete so the fixture
		// it needs is still on disk.
		code, body := do("bob@example.com", bob, http.MethodDelete, entryURL(bobSpace, bobPage, "/"+secret), "")
		if !existsX(t, d, secret) {
			t.Errorf("[X-LEAK-DELETE] LEAK: bob DESTROYED the private page's changelog entry by "+
				"naming HIS OWN page in the URL (%d %s) — the row is gone", code, body)
		}

		// ── X-OWN-DELETE. Over-correction direction, on the row rather than the status.
		if code, body := do("bob@example.com", bob, http.MethodDelete, entryURL(bobSpace, bobPage, "/"+bobEntry), ""); code != http.StatusOK {
			t.Errorf("[X-OWN-DELETE] OVER-CORRECTION: bob cannot delete an entry on HIS OWN page (%d %s)",
				code, body)
		} else if existsX(t, d, bobEntry) {
			t.Errorf("[X-OWN-DELETE] the route answered 200 and the row is still on disk")
		}
	})

	// ── X-GRANT. Granted LAST, so every case above ran while bob was still denied. An explicit
	// view grant on the private space must open the entry AT ITS OWN ADDRESS — which is what
	// rules out a fix shaped "refuse anything whose page is in a private space". The gate
	// decides who may read; the page scope only decides which object the id names.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`, privSpace, bob, ws); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if code, body := do("bob@example.com", bob, http.MethodGet, entryURL(privSpace, privPage, "/"+secret2), ""); code != http.StatusOK {
		t.Errorf("[X-GRANT] OVER-CORRECTION: bob holds an explicit 'view' grant on the private space "+
			"and still cannot read its changelog entry at its own address (%d %s)", code, body)
	}
}

func seedSpaceX(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		wsID, name, "sp-"+name+"-"+wsID, creator, private).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageX(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		spaceID, wsID, title, "pg-"+title+"-"+wsID, author, "body of "+title,
		`{"type":"doc","content":[]}`).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}

// seedEntryX writes summary and content as well as the title: the claim is about the size of the
// payload a borrowed address returns, not about a name being disclosed.
func seedEntryX(t *testing.T, d *testutil.DB, wsID, pageID, author, version, title, summary, content string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO changelog_entries (page_id, workspace_id, version, title, summary, type, content, created_by)
		 VALUES ($1,$2,$3,$4,$5,'feature',$6,$7) RETURNING id`,
		pageID, wsID, version, title, summary, content, author).Scan(&id); err != nil {
		t.Fatalf("seed entry %q: %v", title, err)
	}
	return id
}

// The three oracles below read Postgres directly. Deliberately NOT GetEntry: the same edit that
// breaks the scope would break the reader, and a subject cannot be its own witness.
func summaryOfX(t *testing.T, d *testutil.DB, id string) string {
	t.Helper()
	var s string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT summary FROM changelog_entries WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("read summary of %s: %v", id, err)
	}
	return s
}

func publishedX(t *testing.T, d *testutil.DB, id string) bool {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM changelog_entries WHERE id=$1 AND published_at IS NOT NULL`, id).Scan(&n); err != nil {
		t.Fatalf("read published_at of %s: %v", id, err)
	}
	return n == 1
}

func existsX(t *testing.T, d *testutil.DB, id string) bool {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM changelog_entries WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", id, err)
	}
	return n == 1
}
