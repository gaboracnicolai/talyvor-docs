package mcp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/testutil"
)

// grantEdit gives member an edit grant on a page — the AccessEdit tier the MCP write gate now requires.
// These attribution tests are about WHO a write is credited to, not the tier, so the actor is granted
// edit to keep exercising the write path (wiring the real permission, not weakening the gate).
func grantEdit(t *testing.T, d *testutil.DB, ws, pageID, member, grantedBy string) {
	t.Helper()
	if err := permission.NewStore(d.Pool).Grant(context.Background(), permission.Permission{
		ResourceType: permission.ResourcePage, ResourceID: pageID, SubjectType: "member",
		SubjectID: member, Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: grantedBy,
	}); err != nil {
		t.Fatalf("grant edit: %v", err)
	}
}

// MCP tools took their ACTOR from client-supplied JSON-RPC args:
//
//	create_page  → CreatedBy:  stringArg(args, "created_by",  "agent")
//	update_page  → updates["updated_by"] = stringArg(args, "updated_by", "agent")
//	verify_page  → Verify(..., stringArg(args, "verified_by", "agent"))
//
// Tool args are entirely client-controlled, so these are self-asserted identities. The
// SEC-4 chokepoint in callTool already resolves the VERIFIED actor and stashes it —
// `ctx = authz.WithAuthorized(ctx, m.WorkspaceID, m.MemberID)` — and every tool then
// ignored it. authz.AuthorizedMember was dead code with zero callers repo-wide: the cure
// was written and never wired.
//
// updated_by is not merely cosmetic: page.Store.Update feeds it to the lock guard as the
// editor identity, and pagelock's CanEdit allows the edit when it equals locked_by. So
// naming the lock holder edits through their lock — a privilege bypass with no is_admin
// needed.
//
// RED: an agent edits a page locked by someone else by claiming their id, and attribution
// is whatever the args say. GREEN: the actor is the verified caller; args are ignored.
func TestSecMCP_UpdatePage_ActorIsVerifiedNotArgs(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	mallory := d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Spec")
	grantEdit(t, d, ws, pageID, mallory, alice)

	chain := newMCPChain(t, d)

	// (a) ATTRIBUTION: Mallory updates while claiming to be Alice.
	rr := callTool(chain, "mallory@corp.com", true, "update_page", map[string]any{
		"page_id": pageID, "title": "mallory-was-here", "updated_by": alice,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update_page = %d, want 200 (the tool must work for a member). body=%s", rr.Code, rr.Body.String())
	}
	var updatedBy string
	if err := d.Pool.QueryRow(ctx, `SELECT updated_by FROM pages WHERE id=$1`, pageID).Scan(&updatedBy); err != nil {
		t.Fatal(err)
	}
	if updatedBy == alice {
		t.Errorf("ATTRIBUTION FORGERY: updated_by = %q (Alice) — the tool took the actor from a "+
			"client-supplied arg. The chokepoint already resolved the verified actor via "+
			"authz.WithAuthorized; authz.AuthorizedMember reads it and had zero callers.", updatedBy)
	}
	if updatedBy != mallory {
		t.Errorf("updated_by = %q, want Mallory's verified member id %q", updatedBy, mallory)
	}

	// (b) The default is no better: with no arg at all it used to write the literal
	// "agent" rather than the real caller.
	rr = callTool(chain, "mallory@corp.com", true, "update_page", map[string]any{
		"page_id": pageID, "title": "no-arg",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update_page (no updated_by arg) = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if err := d.Pool.QueryRow(ctx, `SELECT updated_by FROM pages WHERE id=$1`, pageID).Scan(&updatedBy); err != nil {
		t.Fatal(err)
	}
	if updatedBy != mallory {
		t.Errorf("updated_by with no arg = %q, want the verified caller %q (not a literal \"agent\")", updatedBy, mallory)
	}
}

// verify_page stamps a freshness attestation — "this doc is still accurate, checked by X".
// verified_by came from the args, so any caller could attest as anyone.
func TestSecMCP_VerifyPage_VerifierIsVerifiedNotArgs(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	mallory := d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Spec")
	grantEdit(t, d, ws, pageID, mallory, alice)

	chain := newMCPChain(t, d)
	rr := callTool(chain, "mallory@corp.com", true, "verify_page", map[string]any{
		"page_id": pageID, "verified_by": alice,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("verify_page = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}

	var verifiedBy *string
	if err := d.Pool.QueryRow(ctx, `SELECT verified_by FROM pages WHERE id=$1`, pageID).Scan(&verifiedBy); err != nil {
		t.Fatal(err)
	}
	got := ""
	if verifiedBy != nil {
		got = *verifiedBy
	}
	if got == alice {
		t.Errorf("FORGED ATTESTATION: verified_by = %q (Alice) — Mallory attested as someone else "+
			"via a client-supplied arg", got)
	}
	if got != mallory {
		t.Errorf("verified_by = %q, want Mallory's verified member id %q", got, mallory)
	}
}
