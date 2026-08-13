package page_test

// A PAGE COULD BE MADE THE CHILD OF ANOTHER TENANT'S PAGE, THROUGH EITHER DOOR.
//
// `parent_id` is a page id the CALLER supplies. It reaches the column through two writers and
// neither looked at it:
//
//	Create  decoded the whole model.Page from the body, then read `SELECT depth FROM pages
//	        WHERE id = $1` — the parent's depth, from any workspace.
//	Update  has `parent_id` in updatableFields; UpdateInWorkspaces authorizes THE PAGE against
//	        the caller's memberships and never looks at the NEW PARENT at all.
//
// The FK is `parent_id TEXT REFERENCES pages(id)` with no tenancy predicate, so Postgres was
// satisfied by any page in the table.
//
// ⚠ MEASURED ON REAL POSTGRES at e9f6109, both doors, before a line was written: with a page in
// workspace A and a page in workspace B, `Create{SpaceID: A's space, WorkspaceID: A, ParentID:
// B's page}` returned 201 with depth 1 — the depth INHERITED from the other tenant's row — and
// `UpdateInWorkspaces(A's page, {"parent_id": B's page}, [A])` returned 200 with the link
// written.
//
// ⚠ WHY IT IS A DEFECT TODAY AND NOT ONLY A LATENT ONE. `List` still filters `space_id`, so the
// foreign child does not appear in the parent's listing — no read leak was measured, and this
// file does not claim one. What IS live is the write: `Store.Delete` re-parents a deleted page's
// children with `UPDATE pages SET parent_id = $1 WHERE parent_id = $2` and no scope of any kind,
// so a member of workspace B deleting B's page performs a WRITE on workspace A's row. The two
// ids arrive from different places and were authorized by different gates — exactly the sentence
// ai_spend.go's BindAISpend block already writes about `page_id` and `workspace_id`, one file
// over, for the same reason.
//
// ⚠ WHY A PRE-CHECK IS NOT A TIME-OF-CHECK/TIME-OF-USE HOLE HERE, since this item has twice
// found gates that authorized one moment and served another (#114, #115). The fact being checked
// is the parent's `workspace_id`, and that column is NOT WRITABLE: it is absent from
// updatableFields, and no statement in the repository updates it. [WS-IMMUTABLE] below pins that
// premise rather than trusting it — if a later change makes the column writable, this file goes
// red and says why. The one thing that CAN change under the check is the parent being deleted,
// and the FK's ON DELETE SET NULL turns that into a NULL parent, never a cross-tenant one.
//
// ⚠ WHAT IS DELIBERATELY NOT FIXED HERE, so nobody reads a passing file as more than it is:
// CYCLES. `PATCH {"parent_id": <the page itself>}` is accepted, and so is a two-step X→Y→X (both
// measured at e9f6109). A workspace predicate cannot see either — a page is always in its own
// workspace. Preventing them needs a recursive descendant check, which is its own merge; it is
// named in the queue rather than half-covered here. Likewise the SPACE question: a parent in a
// different space of the SAME workspace is still accepted, and whether a page tree may span
// spaces is a product call, not an invariant.

import (
	"context"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

func TestParentMustBeInTheSameWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	pages := page.NewStore(d.Pool)

	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@other.com")

	victim := d.Page(t, wsA, alice, "A Page")
	foreign := d.Page(t, wsB, bob, "B Page")
	var spaceA string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id = $1`, victim).Scan(&spaceA); err != nil {
		t.Fatalf("read A's space: %v", err)
	}
	parentOf := func(id string) string {
		t.Helper()
		var p *string
		if err := d.Pool.QueryRow(ctx, `SELECT parent_id FROM pages WHERE id = $1`, id).Scan(&p); err != nil {
			t.Fatalf("read parent_id: %v", err)
		}
		if p == nil {
			return ""
		}
		return *p
	}
	workspaceOf := func(id string) string {
		t.Helper()
		var w string
		if err := d.Pool.QueryRow(ctx, `SELECT workspace_id FROM pages WHERE id = $1`, id).Scan(&w); err != nil {
			t.Fatalf("read workspace_id: %v", err)
		}
		return w
	}

	// PRECONDITION — the two pages really are in different workspaces.
	if workspaceOf(victim) == workspaceOf(foreign) {
		t.Fatalf("fixture is wrong: both pages are in workspace %s", workspaceOf(victim))
	}

	// [UPDATE-DOOR] the move path, through the workspace-scoped entry point the HTTP handler uses.
	_, updErr := pages.UpdateInWorkspaces(ctx, victim, map[string]any{"parent_id": foreign}, []string{wsA})
	if updErr == nil {
		t.Errorf("[UPDATE-DOOR] UpdateInWorkspaces accepted a parent in another workspace")
	}
	if got := parentOf(victim); got != "" {
		t.Errorf("[UPDATE-DOOR] the cross-tenant link was written: parent_id = %q, want none", got)
	}

	// [CREATE-DOOR] the other writer. Both, because a fix at one door is half a fix — this repo
	// has already shipped that exact half-fix once, on ai_cost_usd's two allowlists.
	created, crErr := pages.Create(ctx, model.Page{
		SpaceID: spaceA, WorkspaceID: wsA, Title: "Created Under B", ParentID: &foreign, CreatedBy: alice,
	})
	if crErr == nil {
		t.Errorf("[CREATE-DOOR] Create accepted a parent in another workspace (new page %s, depth %d)",
			created.ID, created.Depth)
	}
	var orphans int
	if err := d.Pool.QueryRow(ctx,
		`SELECT count(*) FROM pages WHERE title = 'Created Under B'`).Scan(&orphans); err != nil {
		t.Fatalf("count: %v", err)
	}
	if orphans != 0 {
		t.Errorf("[CREATE-DOOR] the refused create still left %d row(s) on disk", orphans)
	}

	// [SAME-ERROR] a parent that does not exist and a parent that is not yours must be the same
	// answer. Otherwise the refusal is an existence oracle for another tenant's page ids — and
	// the missing-parent case used to answer with the raw driver text, FK constraint name and
	// SQLSTATE included.
	_, ghostErr := pages.UpdateInWorkspaces(ctx, victim,
		map[string]any{"parent_id": "00000000-0000-0000-0000-000000000000"}, []string{wsA})
	if ghostErr == nil {
		t.Fatalf("[SAME-ERROR] a nonexistent parent was accepted")
	}
	if updErr != nil && ghostErr.Error() != updErr.Error() {
		t.Errorf("[SAME-ERROR] foreign parent says %q, missing parent says %q — the difference is an oracle",
			updErr, ghostErr)
	}
	for _, leak := range []string{"SQLSTATE", "constraint", "pages_parent_id_fkey"} {
		if strings.Contains(ghostErr.Error(), leak) {
			t.Errorf("[SAME-ERROR] the refusal carries driver text (%q): %v", leak, ghostErr)
		}
	}

	// ── the vacuity floor: a gate that refuses everything would pass every case above ────────
	sibling := d.Page(t, wsA, alice, "A Sibling")
	if _, err := d.Pool.Exec(ctx, `UPDATE pages SET space_id = $1 WHERE id = $2`, spaceA, sibling); err != nil {
		t.Fatalf("move sibling into A's space: %v", err)
	}

	// [SAME-WS-STILL-WORKS] the ordinary move, and the ordinary create.
	if _, err := pages.UpdateInWorkspaces(ctx, victim, map[string]any{"parent_id": sibling}, []string{wsA}); err != nil {
		t.Errorf("[SAME-WS-STILL-WORKS] a same-workspace re-parent was refused: %v", err)
	}
	if got := parentOf(victim); got != sibling {
		t.Errorf("[SAME-WS-STILL-WORKS] parent_id = %q, want %q", got, sibling)
	}
	kid, err := pages.Create(ctx, model.Page{
		SpaceID: spaceA, WorkspaceID: wsA, Title: "Honest Child", ParentID: &sibling, CreatedBy: alice,
	})
	if err != nil {
		t.Errorf("[SAME-WS-STILL-WORKS] a same-workspace create-with-parent was refused: %v", err)
	} else if kid.Depth != 1 {
		t.Errorf("[SAME-WS-STILL-WORKS] depth = %d, want 1 — the gate must not disturb the derivation", kid.Depth)
	}

	// [UNPARENT] clearing the parent is not a parent lookup. A gate that treats nil as "look up
	// the page with id NULL" would make every page permanently un-unparentable.
	if _, err := pages.UpdateInWorkspaces(ctx, victim, map[string]any{"parent_id": nil}, []string{wsA}); err != nil {
		t.Errorf("[UNPARENT] clearing parent_id was refused: %v", err)
	}
	if got := parentOf(victim); got != "" {
		t.Errorf("[UNPARENT] parent_id = %q, want cleared", got)
	}

	// [WS-IMMUTABLE] the premise the whole gate rests on: a page's workspace cannot be moved by
	// a caller, so the fact checked above cannot change under the write that follows it.
	if _, err := pages.UpdateInWorkspaces(ctx, victim, map[string]any{"workspace_id": wsB}, []string{wsA}); err != nil {
		t.Logf("[WS-IMMUTABLE] the patch was refused outright: %v", err)
	}
	if got := workspaceOf(victim); got != wsA {
		t.Errorf("[WS-IMMUTABLE] workspace_id is writable (now %q, was %q) — the parent check above "+
			"is a time-of-check hole the moment that is true", got, wsA)
	}
}
