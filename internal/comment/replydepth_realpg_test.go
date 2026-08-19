package comment_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/testutil"
)

// A REPLY TO A REPLY WAS PROMOTED TO A TOP-LEVEL THREAD BY THE LISTING, SO THE PANEL RENDERED ONE
// MORE CONVERSATION THAN THE COUNT BESIDE IT CLAIMED.
//
// The API accepts it: ReplyInWorkspaces authorizes with assertInPage, which asks only that the
// {id} being replied to is a comment ON the authorized page — a reply is such a comment. Store.Reply
// then INHERITS thread_id from the parent, so the depth-2 row is already correctly filed in its
// thread on disk. Only the read was wrong.
//
// ListByPage bucketed by walking parent pointers into a map that holds HEADS ONLY:
//
//	if c.ParentID == nil { threads[c.ID] = c; heads = append(heads, c) }
//	else if parent, ok := threads[*c.ParentID]; ok { parent.Replies = append(...) }
//	else { heads = append(heads, c) }   // "orphan"
//
// A depth-2 reply's parent is a reply, which is never in `threads`, so every depth-2 row fell into
// the orphan branch and was SURFACED AS ITS OWN THREAD.
//
// ⚠ THE ORPHAN BRANCH'S STATED CAUSE CANNOT HAPPEN, WHICH IS WHY NOBODY SUSPECTED THE BRANCH.
// Its comment reads "parent missing in this query — could be resolved + filtered out". `resolved`
// is written in exactly two statements in this repository (Resolve and Unresolve) and BOTH are
// `WHERE thread_id = (SELECT thread_id ...)` — thread-wide. A head and its replies therefore always
// share `resolved`, so `include_resolved=false` can never drop a parent while keeping a child. The
// branch was reachable by depth-2 replies and by nothing else, and its comment explained a
// different, unreachable case.
//
// WHAT THE USER SAW: GetStats counts `parent_id IS NULL` — the depth-2 row is NOT counted — while
// the list promoted it. The same self-contradiction #171 fixed one field over ("Resolved (1)" above
// a list of three), reached by a different route.
//
// ⚠ AND THE PROMOTED ROW CARRIED THE WRONG CONTROLS. CommentThread renders a "Resolve" button per
// top-level thread, and Resolve is thread-wide — so resolving what the screen presented as its own
// standalone conversation would have settled the OTHER conversation it was still visibly part of.
//
// RED before the fix: three top-level entries beside a count of two. GREEN: two threads, the
// depth-2 reply nested with its sibling under the head they share, count == list.
func TestListByPage_ReplyToAReplyStaysInItsThread_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Deployment runbook")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}
	// Comment participation is the AccessComment tier. Granted explicitly so a tier 403 cannot
	// stand in for the shape this test is about.
	if _, err := permission.NewStore(d.Pool).Grant(ctx, permission.Permission{
		ResourceType: permission.ResourcePage, ResourceID: pageID, SubjectType: "member",
		SubjectID: alice, Access: permission.AccessComment, WorkspaceID: ws, GrantedBy: alice,
	}); err != nil {
		t.Fatalf("grant comment: %v", err)
	}

	base := "/v1/spaces/" + spaceID + "/pages/" + pageID + "/comments"
	chain := cmtChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	post := func(tag, path, body string) comment.Comment {
		t.Helper()
		rr := do(cmtReq(http.MethodPost, path, "alice@corp.com", body))
		if rr.Code != http.StatusCreated {
			t.Fatalf("[%s] POST %s: want 201, got %d — %s", tag, path, rr.Code, rr.Body.String())
		}
		var c comment.Comment
		if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
			t.Fatalf("[%s] decode: %v", tag, err)
		}
		return c
	}

	// Thread one: a head, a reply, and a reply TO THAT REPLY.
	h1 := post("SEED", base, `{"content":"the rollback step is wrong","author_name":"Alice"}`)
	r1 := post("SEED", base+"/"+h1.ID+"/reply", `{"content":"agreed, which line?","author_name":"Alice"}`)
	// [DEPTH-2-ACCEPTED] is a PRECONDITION, not the claim. If the API ever starts refusing a reply
	// to a reply this test must be re-thought rather than silently passing on a fixture that no
	// longer contains the shape it is about.
	r2 := post("DEPTH-2-ACCEPTED", base+"/"+r1.ID+"/reply", `{"content":"line 14","author_name":"Alice"}`)

	// Thread two: an ordinary head + reply. It is here so that "the depth-2 reply was nested" cannot
	// be satisfied by a listing that has simply stopped separating conversations.
	//
	// ⚠ MEASURED, NOT ASSUMED, AND IT CORRECTS WHAT THIS COMMENT FIRST CLAIMED. I predicted that
	// [TWO-THREADS-STAY-TWO] would be the unique catcher for a "file every reply under one head"
	// mutation. It is not: `heads` comes back ordered by thread_id, so the head that ends up empty
	// reds [NESTED] one assertion earlier (control C2). The half of this companion that is
	// load-bearing is that the second head is still A HEAD — held by [TWO-THREADS] and the byID
	// lookup below. Its reply-set equality is redundant under every mutation that could be
	// constructed against this fixture; it is kept as a statement of intent, not claimed as a guard.
	h2 := post("SEED", base, `{"content":"unrelated: the diagram is stale","author_name":"Alice"}`)
	r3 := post("SEED", base+"/"+h2.ID+"/reply", `{"content":"I will redraw it","author_name":"Alice"}`)

	// PRECONDITION FROM RAW SQL, not from the method under test: the depth-2 row really is on disk
	// with a parent that is itself a reply, and it really does carry the head's thread_id. Asserting
	// this with ListByPage would let "Reply filed it wrongly" read as "the listing is fine".
	var parentOfR2, threadOfR2, threadOfH1 string
	if err := d.Pool.QueryRow(ctx,
		`SELECT c.parent_id, c.thread_id, h.thread_id
		   FROM page_comments c, page_comments h
		  WHERE c.id = $1 AND h.id = $2`, r2.ID, h1.ID,
	).Scan(&parentOfR2, &threadOfR2, &threadOfH1); err != nil {
		t.Fatalf("precondition read: %v", err)
	}
	if parentOfR2 != r1.ID {
		t.Fatalf("[PRECONDITION] the depth-2 row must hang off the REPLY; parent_id=%s want %s", parentOfR2, r1.ID)
	}
	if threadOfR2 != threadOfH1 {
		t.Fatalf("[PRECONDITION] Store.Reply must inherit the head's thread_id; got %s want %s", threadOfR2, threadOfH1)
	}

	// THE READ, through the shipped route.
	rr := do(cmtReq(http.MethodGet, base, "alice@corp.com", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET comments: %d — %s", rr.Code, rr.Body.String())
	}
	var list []comment.Comment
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}

	if len(list) != 2 {
		got := make([]string, 0, len(list))
		for _, c := range list {
			got = append(got, c.Content)
		}
		t.Fatalf("[TWO-THREADS] the page holds exactly two conversations; the listing returned %d top-level entries: %v", len(list), got)
	}

	byID := map[string]comment.Comment{}
	for _, c := range list {
		byID[c.ID] = c
	}
	first, ok := byID[h1.ID]
	if !ok {
		t.Fatalf("[TWO-THREADS] the first head is missing from the listing: %v", byID)
	}
	second, ok := byID[h2.ID]
	if !ok {
		t.Fatalf("[TWO-THREADS-STAY-TWO] the second head is missing from the listing: %v", byID)
	}

	replyIDs := func(c comment.Comment) map[string]bool {
		out := map[string]bool{}
		for _, r := range c.Replies {
			out[r.ID] = true
		}
		return out
	}
	got1 := replyIDs(first)
	if !got1[r1.ID] {
		t.Fatalf("[NESTED] the depth-1 reply must nest under its head; got %v", got1)
	}
	if !got1[r2.ID] {
		t.Fatalf("[NESTED-DEPTH-2] the reply TO THE REPLY must nest under the head whose thread_id it "+
			"already carries — promoting it makes the panel render a conversation that does not exist; got %v", got1)
	}
	if len(got1) != 2 {
		t.Fatalf("[NESTED-DEPTH-2] the first thread holds exactly its two replies; got %d %v", len(got1), got1)
	}
	got2 := replyIDs(second)
	if len(got2) != 1 || !got2[r3.ID] {
		t.Fatalf("[TWO-THREADS-STAY-TWO] the second thread keeps its own single reply and gains nothing; got %v", got2)
	}

	// THE COUNT AND THE LIST ARE THE TWO HALVES OF ONE SCREEN, so they are asserted against each
	// other rather than against a constant — a fixture that grew a thread would otherwise make this
	// pass for the wrong reason.
	sr := do(cmtReq(http.MethodGet, base+"/stats", "alice@corp.com", ""))
	if sr.Code != http.StatusOK {
		t.Fatalf("GET stats: %d — %s", sr.Code, sr.Body.String())
	}
	var stats comment.CommentStats
	if err := json.Unmarshal(sr.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Total != len(list) {
		t.Fatalf("[COUNT-MATCHES-LIST] the panel's count and its list must describe the same page: "+
			"stats.total=%d, list=%d entries", stats.Total, len(list))
	}
	if stats.Open != len(list) {
		t.Fatalf("[COUNT-MATCHES-LIST] nothing here is resolved, so open must equal the list too: "+
			"stats.open=%d, list=%d entries", stats.Open, len(list))
	}
}
