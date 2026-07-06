package page_test

import (
	"context"
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/comment"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// SEC-4 Layer 2 for space + comment: the workspace-scoped store variants deny a caller whose
// membership set doesn't include the object's workspace (space via workspace_id; comment via
// its page's workspace), and allow the real member. Store-level so it exercises the scope guard
// directly, independent of routing.
func TestSEC4_Scoping_SpaceAndComment(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsA := d.Workspace(t) // Alice's workspace
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pB := d.Page(t, wsB, bob, "B doc")
	sB := spaceOf(t, d, pB)
	// Seed the comment via direct SQL: comment.Create has a pre-existing ambiguous-`id` bug
	// on real PG (a WITH…UPDATE…RETURNING that pgxmock never executed) — separate from SEC-4.
	var cmtID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO page_comments (page_id, author_id, author_name, content) VALUES ($1,$2,$3,$4) RETURNING id`,
		pB, bob, "Bob", "secret note").Scan(&cmtID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}

	aliceScope := []string{wsA} // Alice belongs to A only
	bobScope := []string{wsB}

	// SPACE: A-scoped read of B's space → ErrNotFound; B-scoped → ok.
	spaceStore := space.NewStore(d.Pool)
	if _, err := spaceStore.GetByIDInWorkspaces(ctx, sB, aliceScope); !errors.Is(err, space.ErrNotFound) {
		t.Errorf("space cross-tenant read = %v, want space.ErrNotFound", err)
	}
	if err := spaceStore.DeleteInWorkspaces(ctx, sB, aliceScope); !errors.Is(err, space.ErrNotFound) {
		t.Errorf("space cross-tenant delete = %v, want space.ErrNotFound", err)
	}
	if _, err := spaceStore.GetByIDInWorkspaces(ctx, sB, bobScope); err != nil {
		t.Errorf("space same-tenant read = %v, want ok", err)
	}

	// COMMENT: A-scoped ops on B's comment → ErrNotFound; B-scoped resolve → ok.
	commentStore := comment.NewStore(d.Pool)
	if err := commentStore.DeleteInWorkspaces(ctx, cmtID, bob, aliceScope); !errors.Is(err, comment.ErrNotFound) {
		t.Errorf("comment cross-tenant delete = %v, want comment.ErrNotFound", err)
	}
	if err := commentStore.ResolveInWorkspaces(ctx, cmtID, bob, aliceScope); !errors.Is(err, comment.ErrNotFound) {
		t.Errorf("comment cross-tenant resolve = %v, want comment.ErrNotFound", err)
	}
	if err := commentStore.ResolveInWorkspaces(ctx, cmtID, bob, bobScope); err != nil {
		t.Errorf("comment same-tenant resolve = %v, want ok", err)
	}
}
