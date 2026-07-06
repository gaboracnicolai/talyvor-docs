package comment_test

import (
	"context"
	"testing"

	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/testutil"
)

// comment.Create builds thread_id from the freshly-inserted row id via a
// `WITH inserted AS (... RETURNING id) UPDATE page_comments ... RETURNING id, ...` CTE.
// On real Postgres the unqualified `id` in the final RETURNING is ambiguous between
// page_comments and the `inserted` CTE (both expose `id`) → error 42702. The package's
// pgxmock tests never executed the SQL, so this only fails on a real DB — which the
// real-PG harness now catches.
//
// RED (pre-fix): Create fails with "column reference \"id\" is ambiguous".
// GREEN (post-fix): Create succeeds; a top-level comment is its own thread root, and a
// reply inherits the parent's thread.
func TestCreate_TopLevel_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	pageID := d.Page(t, ws, author, "Doc")

	store := comment.NewStore(d.Pool)
	c, err := store.Create(ctx, pageID, nil, author, "Author", "first comment")
	if err != nil {
		t.Fatalf("Create top-level comment: %v", err)
	}
	if c.ThreadID == nil || *c.ThreadID != c.ID {
		t.Fatalf("top-level comment must be its own thread root: id=%s thread_id=%v", c.ID, c.ThreadID)
	}
	if c.Content != "first comment" {
		t.Fatalf("content = %q, want %q", c.Content, "first comment")
	}
}

func TestCreate_Reply_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "author@corp.com")
	pageID := d.Page(t, ws, author, "Doc")

	store := comment.NewStore(d.Pool)
	parent, err := store.Create(ctx, pageID, nil, author, "Author", "parent")
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	reply, err := store.Reply(ctx, parent.ID, author, "Author", "a reply")
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.ThreadID == nil || *reply.ThreadID != *parent.ThreadID {
		t.Fatalf("reply must inherit the parent's thread: parent_thread=%v reply_thread=%v", parent.ThreadID, reply.ThreadID)
	}
	if reply.ParentID == nil || *reply.ParentID != parent.ID {
		t.Fatalf("reply parent_id = %v, want %s", reply.ParentID, parent.ID)
	}
}
