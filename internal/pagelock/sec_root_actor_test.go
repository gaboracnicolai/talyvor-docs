package pagelock_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/testutil"
)

// The SECURITY half of the root fix — the exact repro found (and, lacking a fix, removed)
// during Run 1.
//
// memberFromReq(r, in.MemberID) preferred the verified actor but FELL BACK to the request
// body's member_id when the context actor was empty. authz.ActorOrEmpty → SingleMemberID
// returns "" for ANY caller with != 1 memberships — so for a member of TWO workspaces the
// fallback was live and the BODY named the actor. Store.Unlock then compares that name to
// locked_by: claim the locker's id and the lock is yours.
//
// Reaching the Edit-gated route with an empty actor needed an `everyone: edit` grant,
// because no subject_type='member' grant can match "" — an ordinary setting for an
// internal wiki, and the same condition that made the caller's grants evaporate.
//
// The root fix removes the precondition entirely: permission.ActorFromContext resolves the
// caller's member id in the PAGE's workspace from the verified membership set, correct for
// any membership count, so there is nothing to fall back to.
//
// RED: Mallory (2 workspaces) unlocks Bob's lock by sending {"member_id": "<bob>"} → 200.
// GREEN: 403, Bob's lock survives, and Bob himself can still unlock.
func TestSecRoot_Unlock_MultiWorkspaceCallerCannotForgeMemberID(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)

	alice := d.Member(t, wsA, "alice@corp.com") // space creator → admin
	bob := d.Member(t, wsA, "bob@corp.com")     // ONE workspace → verified actor resolves today
	// Mallory belongs to BOTH → pre-fix, SingleMemberID returns "" for her.
	mallory := d.Member(t, wsA, "mallory@corp.com")
	d.Member(t, wsB, "mallory@corp.com")

	pageID := d.Page(t, wsA, alice, "Locked spec")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}

	// `everyone: edit` — the only grant shape that could match an empty memberID, and the
	// condition that made the fallback reachable.
	permStore := permission.NewStore(d.Pool)
	if err := permStore.Grant(ctx, permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: spaceID,
		SubjectType: "everyone", SubjectID: "*",
		Access: permission.AccessEdit, WorkspaceID: wsA, GrantedBy: alice,
	}); err != nil {
		t.Fatalf("grant everyone edit: %v", err)
	}

	chain := v1LockChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	lockPath := "/v1/spaces/" + spaceID + "/pages/" + pageID + "/lock"

	lockedBy := func() string {
		t.Helper()
		var by *string
		if err := d.Pool.QueryRow(ctx, `SELECT locked_by FROM pages WHERE id=$1`, pageID).Scan(&by); err != nil {
			t.Fatalf("read locked_by: %v", err)
		}
		if by == nil {
			return ""
		}
		return *by
	}

	// Bob takes the lock.
	if rr := do(lockReq(http.MethodPost, lockPath, "bob@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Fatalf("Bob lock = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != bob {
		t.Fatalf("locked_by = %q, want Bob %q — fixture wrong", got, bob)
	}

	// THE FORGERY: Mallory names Bob as the actor.
	rr := do(lockReq(http.MethodDelete, lockPath, "mallory@corp.com", `{"member_id":"`+bob+`"}`))
	if rr.Code != http.StatusForbidden {
		t.Errorf("Mallory (2 workspaces) unlock with forged {\"member_id\":\"<bob>\"} = %d, want 403 "+
			"(the actor must be resolved from the verified identity in the page's workspace, "+
			"never named by the body). body=%s", rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != bob {
		t.Errorf("IDENTITY FORGERY: Mallory stole Bob's lock by claiming his member_id — "+
			"locked_by is now %q, want Bob %q", got, bob)
	}

	// Claiming Alice (a real admin) must not work either.
	if rr := do(lockReq(http.MethodDelete, lockPath, "mallory@corp.com", `{"member_id":"`+alice+`"}`)); rr.Code != http.StatusForbidden {
		t.Errorf("Mallory unlock claiming the admin's member_id = %d, want 403", rr.Code)
	}
	if got := lockedBy(); got != bob {
		t.Errorf("after admin-id forgery, locked_by = %q, want Bob %q", got, bob)
	}

	// SCOPE, NOT BREAKAGE: Mallory can still lock/unlock as HERSELF. A fix that simply
	// deleted the fallback would fail-close her out of the feature entirely — this asserts
	// the actor resolves for her rather than vanishing.
	if rr := do(lockReq(http.MethodDelete, lockPath, "bob@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Fatalf("Bob (the locker) unlock = %d, want 200", rr.Code)
	}
	if rr := do(lockReq(http.MethodPost, lockPath, "mallory@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Errorf("Mallory (2 workspaces) locking as HERSELF = %d, want 200 — the fix must "+
			"resolve her actor, not fail her closed out of locking. body=%s", rr.Code, rr.Body.String())
	}
	if got := lockedBy(); got != mallory {
		t.Errorf("locked_by = %q, want Mallory's real member id in wsA %q", got, mallory)
	}
	if rr := do(lockReq(http.MethodDelete, lockPath, "mallory@corp.com", `{}`)); rr.Code != http.StatusOK {
		t.Errorf("Mallory unlocking her OWN lock = %d, want 200", rr.Code)
	}
}
