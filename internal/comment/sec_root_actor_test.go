package comment_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// The comment half of the root: the SAME memberFromReq(r, in.AuthorID) body fallback as
// pagelock. authz.ActorOrEmpty returns "" for any caller with != 1 memberships, so for a
// multi-workspace member the request BODY named the comment's author — and comment
// authorship is load-bearing: Store.Delete gates on "only the author can delete".
//
// Both halves are broken today, in opposite directions:
//   SECURITY:   a two-workspace member posts a comment authored as someone else.
//   FUNCTIONAL: the same member posting honestly gets author_id "" — an unattributable
//               comment they cannot subsequently delete.
//
// GREEN: the author is permission.ActorFromContext — the caller's member id in the page's
// workspace, resolved from the verified identity by the RequireAccess middleware that
// gates every route in comment's Mount.

const cmtSecret = "sec4-test-gateway-secret-0123456789"

func cmtChain(d *testutil.DB) http.Handler {
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
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := comment.NewHandler(comment.NewStore(d.Pool)).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(cmtSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func cmtReq(method, path, email, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Gateway-Auth", cmtSecret)
	r.Header.Set("X-User-Email", email)
	return r
}

func TestSecRoot_Comment_AuthorIsVerifiedNotBody(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	bob := d.Member(t, wsA, "bob@corp.com")
	// Mallory: two workspaces → pre-fix ActorOrEmpty is "" for her.
	mallory := d.Member(t, wsA, "mallory@corp.com")
	d.Member(t, wsB, "mallory@corp.com")

	pageID := d.Page(t, wsA, alice, "Spec")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	base := "/v1/spaces/" + spaceID + "/pages/" + pageID + "/comments"

	chain := cmtChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	authorOf := func(content string) string {
		t.Helper()
		var a string
		if err := d.Pool.QueryRow(ctx, `SELECT author_id FROM page_comments WHERE content=$1`, content).Scan(&a); err != nil {
			t.Fatalf("read author for %q: %v", content, err)
		}
		return a
	}

	// (a) THE FORGERY: Mallory posts a comment claiming Bob wrote it.
	if rr := do(cmtReq(http.MethodPost, base, "mallory@corp.com",
		`{"content":"forged-as-bob","author_id":"`+bob+`"}`)); rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("Mallory create = %d, want 2xx. body=%s", rr.Code, rr.Body.String())
	}
	if got := authorOf("forged-as-bob"); got == bob {
		t.Errorf("ATTRIBUTION FORGERY: comment stored with author_id = Bob's id %q — the body "+
			"named the author. Comment authorship gates deletion (\"only the author can delete\").", got)
	} else if got != mallory {
		t.Errorf("author_id = %q, want Mallory's real member id in wsA %q", got, mallory)
	}

	// (b) FUNCTIONAL: posting honestly must attribute to her, not to "".
	if rr := do(cmtReq(http.MethodPost, base, "mallory@corp.com", `{"content":"honest"}`)); rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("Mallory honest create = %d, want 2xx. body=%s", rr.Code, rr.Body.String())
	}
	if got := authorOf("honest"); got != mallory {
		t.Errorf("honest comment author_id = %q, want Mallory %q — a multi-workspace member's "+
			"comment must be attributable (an empty author cannot later delete their own comment)", got, mallory)
	}

	// (c) A single-workspace member is unaffected — control.
	if rr := do(cmtReq(http.MethodPost, base, "bob@corp.com", `{"content":"bob-comment"}`)); rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("Bob create = %d, want 2xx", rr.Code)
	}
	if got := authorOf("bob-comment"); got != bob {
		t.Errorf("CONTROL: Bob's comment author_id = %q, want %q", got, bob)
	}

	// (d) Mallory can delete her OWN comment — proves the resolved actor matches what was
	// stored, i.e. she is not locked out of her own content.
	var honestID string
	if err := d.Pool.QueryRow(ctx, `SELECT id FROM page_comments WHERE content='honest'`).Scan(&honestID); err != nil {
		t.Fatal(err)
	}
	if rr := do(cmtReq(http.MethodDelete, base+"/"+honestID, "mallory@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Errorf("Mallory deleting her OWN comment = %d, want 200 (a multi-workspace member "+
			"must not be locked out of their own content). body=%s", rr.Code, rr.Body.String())
	}

	// (e) …and cannot delete Bob's.
	var bobID string
	if err := d.Pool.QueryRow(ctx, `SELECT id FROM page_comments WHERE content='bob-comment'`).Scan(&bobID); err != nil {
		t.Fatal(err)
	}
	if rr := do(cmtReq(http.MethodDelete, base+"/"+bobID, "mallory@corp.com", `{}`)); rr.Code == http.StatusOK {
		t.Errorf("Mallory deleted Bob's comment = 200, want a denial (only the author may delete)")
	}
}
