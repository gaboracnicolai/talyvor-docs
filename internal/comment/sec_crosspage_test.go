package comment_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/testutil"
)

// The last known scoping gap. Different class from #23's client-supplied authority: nothing
// here is forged. The caller authenticates honestly, the route authorizes honestly — and
// then the store acts on a DIFFERENT resource than the one that was authorized.
//
// The comment routes are /spaces/{spaceID}/pages/{pageID}/comments/{id}/..., gated by
// pageEnf.Require on {pageID}. But the store's scope check is:
//
//	assertInWorkspaces(commentID, wsIDs)
//	  SELECT 1 FROM page_comments c JOIN pages p ON c.page_id = p.id
//	  WHERE c.id = $1 AND p.workspace_id = ANY($2)
//
// It asserts the comment's page is SOMEWHERE in the caller's workspace set — never that it
// is the {pageID} the route just authorized. So {pageID} chooses the permission check and
// {id} chooses the victim, and they are never tied together.
//
// Blast radius: cross-page WITHIN a tenant (cross-tenant is still blocked by the ANY($2)
// filter). That is smaller than #23's, which is why it was deferred twice — but it is not
// nothing: a member with View on ANY public page can reach a comment on a PRIVATE page they
// have no grant for and cannot read. Structurally identical to the ce8bfe3 share-revoke bug.
//
// RED: Mallory resolves/deletes a comment on a page she cannot read, by authorizing against
// a page she can. GREEN: 404, and the legitimate same-page operations still work.
func TestSec_Comment_CannotActAcrossPagesViaAuthorizedPageID(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	mallory := d.Member(t, ws, "mallory@corp.com")

	// Public page — Mallory gets View by the public-space default.
	publicPage := d.Page(t, ws, alice, "Public onboarding")
	var publicSpace string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, publicPage).Scan(&publicSpace); err != nil {
		t.Fatal(err)
	}
	// Comment participation now requires the AccessComment tier (was View). Grant Mallory comment on
	// the PUBLIC page so she clears the tier gate and this test still exercises the CROSS-PAGE scoping
	// (its actual subject) rather than being deflected by a tier 403. She still has NO access to the
	// private page — the point of the test is that she reaches its comment via the public {pageID}.
	if err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourcePage, ResourceID: publicPage, SubjectType: "member",
		SubjectID: mallory, Access: permission.AccessComment, WorkspaceID: ws, GrantedBy: alice,
	}); err != nil {
		t.Fatalf("grant comment on public page: %v", err)
	}

	// Private page in a DIFFERENT space — Mallory has no grant, so no access at all.
	privatePage := d.Page(t, ws, alice, "Private compensation review")
	var privateSpace string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, privatePage).Scan(&privateSpace); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE spaces SET private = true WHERE id=$1`, privateSpace); err != nil {
		t.Fatal(err)
	}

	chain := cmtChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	// Bob (who can reach the private page — he is not the point of this test) leaves a
	// comment on it. Seed directly so the fixture does not depend on Bob's grants.
	var secretComment string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO page_comments (page_id, author_id, content, thread_id)
		 VALUES ($1, $2, 'salary discussion', gen_random_uuid()::text) RETURNING id`,
		privatePage, bob).Scan(&secretComment); err != nil {
		t.Fatalf("seed comment on the private page: %v", err)
	}

	// ANCHOR: Mallory genuinely cannot reach the private page's own comment route. This is
	// what makes the cross-page reach below a privilege gain rather than a no-op.
	if rr := do(cmtReq(http.MethodPost,
		"/v1/spaces/"+privateSpace+"/pages/"+privatePage+"/comments/"+secretComment+"/resolve",
		"mallory@corp.com", `{}`)); rr.Code == http.StatusOK {
		t.Fatalf("fixture wrong: Mallory can already reach the private page directly (%d)", rr.Code)
	}

	resolved := func() bool {
		t.Helper()
		var v bool
		if err := d.Pool.QueryRow(ctx, `SELECT resolved FROM page_comments WHERE id=$1`, secretComment).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	// (a) THE BUG: authorize against the PUBLIC page, act on the PRIVATE page's comment.
	rr := do(cmtReq(http.MethodPost,
		"/v1/spaces/"+publicSpace+"/pages/"+publicPage+"/comments/"+secretComment+"/resolve",
		"mallory@corp.com", `{}`))
	if rr.Code != http.StatusNotFound {
		t.Errorf("resolve of a FOREIGN page's comment via an authorized {pageID} = %d, want 404. "+
			"{pageID} picked the permission check and {id} picked the victim, and nothing tied them "+
			"together. body=%s", rr.Code, rr.Body.String())
	}
	if resolved() {
		t.Errorf("Mallory RESOLVED a comment on a private page she cannot read, by authorizing " +
			"against an unrelated public page")
	}

	// (b) Same shape on delete — and delete is destructive.
	rr = do(cmtReq(http.MethodDelete,
		"/v1/spaces/"+publicSpace+"/pages/"+publicPage+"/comments/"+secretComment,
		"mallory@corp.com", `{}`))
	if rr.Code == http.StatusOK {
		t.Errorf("Mallory DELETED a foreign page's comment via an authorized {pageID} (%d)", rr.Code)
	}
	var still int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM page_comments WHERE id=$1`, secretComment).Scan(&still); err != nil {
		t.Fatal(err)
	}
	if still != 1 {
		t.Errorf("the private page's comment was destroyed by a caller authorized against a different page")
	}

	// (c) Same shape on reply — a reply would also LEAK the thread into the wrong page.
	rr = do(cmtReq(http.MethodPost,
		"/v1/spaces/"+publicSpace+"/pages/"+publicPage+"/comments/"+secretComment+"/reply",
		"mallory@corp.com", `{"content":"injected reply"}`))
	if rr.Code == http.StatusOK || rr.Code == http.StatusCreated {
		t.Errorf("Mallory REPLIED into a foreign page's thread via an authorized {pageID} (%d)", rr.Code)
	}

	// (d) SCOPE, NOT BREAKAGE: the legitimate same-page path must still work. A fix that
	// simply denies everything would pass (a)-(c) while breaking comments outright.
	var ownComment string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO page_comments (page_id, author_id, content, thread_id)
		 VALUES ($1, $2, 'on the public page', gen_random_uuid()::text) RETURNING id`,
		publicPage, alice).Scan(&ownComment); err != nil {
		t.Fatal(err)
	}
	if rr := do(cmtReq(http.MethodPost,
		"/v1/spaces/"+publicSpace+"/pages/"+publicPage+"/comments/"+ownComment+"/resolve",
		"mallory@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Errorf("resolving a comment that genuinely belongs to the authorized page = %d, want 200 "+
			"(the fix must tie {id} to {pageID}, not deny everything). body=%s", rr.Code, rr.Body.String())
	}
}
