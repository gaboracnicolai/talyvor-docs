package permission

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	// EVERY EXPECTATION IN THIS PACKAGE IS VERIFIED, NOT JUST THE ONES SOMEBODY REMEMBERED.
	//
	// 3 of this package's 7 expectations were unverified. Control E3 deleted Grant's
	// INSERT and reddened EIGHTEEN tests in TEN packages — this is the authorization spine, and
	// the blast radius is the reason the check belongs on the constructor rather than in the
	// three tests that happen to have it.
	//
	// pgxmock ignores an expectation that was never called unless you ASK it, and this package
	// asked PER TEST, where somebody remembered — which is the shape that leaves the next test
	// written uncovered. Measured by scripts/w31-partial-coverage-write-controls.py, family E.
	//
	// Registered AFTER pool.Close so it runs BEFORE it (t.Cleanup is LIFO). t.Errorf, not
	// t.Fatalf: a cleanup must not Goexit out of another cleanup.
	t.Cleanup(func() {
		if err := pool.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet or mismatched pgxmock expectations: %v", err)
		}
	})
	return newStore(pool), pool
}

// grantRows is the shape RETURNING hands back — the `cols` list, in order. Built here rather than
// in each test so a column added to `cols` fails in ONE place instead of drifting per test.
func grantRows(id string, created time.Time) *pgxmock.Rows {
	return pgxmock.NewRows([]string{
		"id", "resource_type", "resource_id", "subject_type", "subject_id",
		"access", "workspace_id", "granted_by", "created_at",
	}).AddRow(id, "space", "sp-1", "member", "u-1", "edit", "ws-1", "u-admin", created)
}

func TestGrant_UpsertsPermissionRow(t *testing.T) {
	store, pool := newMockStore(t)

	// ExpectQuery, not ExpectExec: Grant now RETURNs the persisted row, because the create route's
	// 201 must carry the id its sibling DELETE route takes. See Grant's doc comment.
	created := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	pool.ExpectQuery(`INSERT INTO permissions`).
		WithArgs("space", "sp-1", "member", "u-1", "edit", "ws-1", "u-admin").
		WillReturnRows(grantRows("perm-1", created))

	out, err := store.Grant(context.Background(), Permission{
		ResourceType: ResourceSpace,
		ResourceID:   "sp-1",
		SubjectType:  "member",
		SubjectID:    "u-1",
		Access:       AccessEdit,
		WorkspaceID:  "ws-1",
		GrantedBy:    "u-admin",
	})
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// THE RETURNED ROW IS THE POINT, so it is asserted rather than discarded: a Grant that runs the
	// right statement and then hands back the struct it was given is the exact defect this replaced.
	if out == nil {
		t.Fatal("Grant returned a nil permission on success")
	}
	if out.ID != "perm-1" {
		t.Errorf("Grant returned id %q, want the RETURNING row's %q", out.ID, "perm-1")
	}
	if !out.CreatedAt.Equal(created) {
		t.Errorf("Grant returned created_at %v, want the RETURNING row's %v", out.CreatedAt, created)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestGrant_RejectsInvalidAccess(t *testing.T) {
	store, _ := newMockStore(t)
	_, err := store.Grant(context.Background(), Permission{
		ResourceType: ResourceSpace,
		ResourceID:   "sp-1",
		SubjectType:  "member",
		SubjectID:    "u-1",
		Access:       "godmode",
		WorkspaceID:  "ws-1",
	})
	if err == nil {
		t.Fatal("expected error for invalid access level")
	}
}

func TestGrant_RejectsTeamTheGranterDoesNotManage(t *testing.T) {
	// A team grant is accepted only from the team's manager in the grant's workspace; anything else
	// is refused before the INSERT. `.Maybe()` on the INSERT: the result this test proves is that it
	// never happens.
	store, pool := newMockStore(t)
	pool.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM teams WHERE id = \$1 AND workspace_id = \$2 AND created_by = \$3\)`).
		WithArgs("t-eng", "ws-1", "u-admin").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))
	pool.ExpectQuery(`INSERT INTO permissions`).
		WithArgs("space", "sp-1", "team", "t-eng", "view", "ws-1", "u-admin").
		WillReturnRows(grantRows("perm-team", time.Now())).Maybe()
	_, err := store.Grant(context.Background(), Permission{
		ResourceType: ResourceSpace, ResourceID: "sp-1",
		SubjectType: "team", SubjectID: "t-eng",
		Access: AccessView, WorkspaceID: "ws-1", GrantedBy: "u-admin",
	})
	if err == nil {
		t.Fatal("Grant accepted a team the granter does not manage")
	}
}

func TestGrant_RejectsAdminForATeam(t *testing.T) {
	// Admin would let a roster one member controls re-share the resource; refused before any query.
	store, _ := newMockStore(t)
	_, err := store.Grant(context.Background(), Permission{
		ResourceType: ResourceSpace, ResourceID: "sp-1",
		SubjectType: "team", SubjectID: "t-eng",
		Access: AccessAdmin, WorkspaceID: "ws-1", GrantedBy: "u-admin",
	})
	if err == nil {
		t.Fatal("Grant gave a team admin")
	}
}

func TestGrant_AcceptsEveryone(t *testing.T) {
	// The removal must not over-reject: "everyone" is a real, honored subject type.
	store, pool := newMockStore(t)
	pool.ExpectQuery(`INSERT INTO permissions`).
		WithArgs("space", "sp-1", "everyone", "everyone", "view", "ws-1", "u-admin").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "resource_type", "resource_id", "subject_type", "subject_id",
			"access", "workspace_id", "granted_by", "created_at",
		}).AddRow("perm-everyone", "space", "sp-1", "everyone", "everyone", "view", "ws-1", "u-admin", time.Now()))
	_, err := store.Grant(context.Background(), Permission{
		ResourceType: ResourceSpace, ResourceID: "sp-1",
		SubjectType: "everyone", SubjectID: "everyone",
		Access: AccessView, WorkspaceID: "ws-1", GrantedBy: "u-admin",
	})
	if err != nil {
		t.Fatalf("Grant rejected subject_type=everyone: %v", err)
	}
}

func TestResolveAccess_TeamGrant_IsIgnored(t *testing.T) {
	// A team grant confers nothing on a caller who is not on the team — even one whose member id
	// happens to equal the team id.
	got := resolveAccess(resourceContext{Type: ResourceSpace, Private: true}, "t-eng", []Permission{
		{SubjectType: "team", SubjectID: "t-eng", Access: AccessAdmin},
	})
	if got != AccessNone {
		t.Fatalf("team grant conferred %q on a non-member, want none", got)
	}
}

func TestRevoke_DeletesByResourceAndSubject(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectExec(`DELETE FROM permissions`).
		WithArgs("space", "sp-1", "member", "u-1").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := store.Revoke(context.Background(), ResourceSpace, "sp-1", "member", "u-1"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRevokeByID_ScopedToWorkspaces_Deletes(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectExec(`DELETE FROM permissions\s+WHERE id = \$1 AND resource_type = \$2 AND resource_id = \$3 AND workspace_id = ANY\(\$4\)`).
		WithArgs("p-1", "space", "sp-1", []string{"ws-1"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	if err := store.RevokeByID(context.Background(), "p-1", ResourceSpace, "sp-1", []string{"ws-1"}); err != nil {
		t.Fatalf("RevokeByID: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestRevokeByID_ForeignWorkspace_ReturnsErrNotFound(t *testing.T) {
	store, pool := newMockStore(t)

	// 0 rows affected — the grant is outside the caller's workspaces.
	pool.ExpectExec(`DELETE FROM permissions\s+WHERE id = \$1 AND resource_type = \$2 AND resource_id = \$3 AND workspace_id = ANY\(\$4\)`).
		WithArgs("p-foreign", "space", "sp-1", []string{"ws-1"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	if err := store.RevokeByID(context.Background(), "p-foreign", ResourceSpace, "sp-1", []string{"ws-1"}); err != ErrNotFound {
		t.Fatalf("want ErrNotFound for a grant outside the caller's workspaces, got %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestListForResource_ReturnsAllGrants(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`SELECT.*FROM permissions WHERE resource_type.*workspace_id = ANY`).
		WithArgs("space", "sp-1", []string{"ws-1"}).
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "resource_type", "resource_id", "subject_type", "subject_id",
			"access", "workspace_id", "granted_by", "created_at",
		}).
			AddRow("p-1", "space", "sp-1", "member", "u-1", "edit", "ws-1", "u-admin", now).
			AddRow("p-2", "space", "sp-1", "team", "t-1", "view", "ws-1", "u-admin", now))

	out, err := store.ListForResource(context.Background(), ResourceSpace, "sp-1", []string{"ws-1"})
	if err != nil {
		t.Fatalf("ListForResource: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out))
	}
	if out[0].Access != AccessEdit {
		t.Fatalf("access wrong: %+v", out[0])
	}
}

// resolveAccess is the rule evaluator the Check() public surface
// wraps. We test it directly so the threshold logic can be
// exercised without spinning up the whole resource lookup graph.

func TestResolveAccess_NoPermissions_PrivateSpace_ReturnsNone(t *testing.T) {
	got := resolveAccess(resourceContext{
		Type:    ResourceSpace,
		Private: true,
	}, "u-1", nil)
	if got != AccessNone {
		t.Fatalf("want none on private space without perms, got %q", got)
	}
}

func TestResolveAccess_NoPermissions_PublicSpace_ReturnsView(t *testing.T) {
	// Public space: workspace members get view by default. The store
	// caller passes Private=false; the evaluator hands back AccessView.
	got := resolveAccess(resourceContext{
		Type:    ResourceSpace,
		Private: false,
	}, "u-1", nil)
	if got != AccessView {
		t.Fatalf("want view on public space, got %q", got)
	}
}

func TestResolveAccess_CreatorIsAdmin(t *testing.T) {
	got := resolveAccess(resourceContext{
		Type:      ResourceSpace,
		Private:   true,
		CreatedBy: "u-1",
	}, "u-1", nil)
	if got != AccessAdmin {
		t.Fatalf("creator must be admin, got %q", got)
	}
}

func TestResolveAccess_MemberPermissionWins(t *testing.T) {
	perms := []Permission{
		{SubjectType: "everyone", SubjectID: "everyone", Access: AccessView},
		{SubjectType: "member", SubjectID: "u-1", Access: AccessEdit},
	}
	got := resolveAccess(resourceContext{Type: ResourceSpace, Private: true}, "u-1", perms)
	if got != AccessEdit {
		t.Fatalf("want edit (member-specific), got %q", got)
	}
}

func TestResolveAccess_EveryoneAppliesToAll(t *testing.T) {
	perms := []Permission{
		{SubjectType: "everyone", SubjectID: "everyone", Access: AccessComment},
	}
	got := resolveAccess(resourceContext{Type: ResourceSpace, Private: true}, "u-99", perms)
	if got != AccessComment {
		t.Fatalf("want comment via everyone, got %q", got)
	}
}

func TestResolveAccess_AdminBeatsLowerExplicit(t *testing.T) {
	perms := []Permission{
		{SubjectType: "member", SubjectID: "u-1", Access: AccessView},
		{SubjectType: "everyone", SubjectID: "everyone", Access: AccessAdmin},
	}
	got := resolveAccess(resourceContext{Type: ResourceSpace, Private: true}, "u-1", perms)
	if got != AccessAdmin {
		t.Fatalf("want admin (highest wins), got %q", got)
	}
}

func TestAccessLevelRank_IsMonotonic(t *testing.T) {
	if !(rank(AccessNone) < rank(AccessView) && rank(AccessView) < rank(AccessComment) &&
		rank(AccessComment) < rank(AccessEdit) && rank(AccessEdit) < rank(AccessAdmin)) {
		t.Fatalf("rank ordering broken")
	}
}

func TestPageInheritsFromSpace_WhenNoPagePermissions(t *testing.T) {
	// For a page with no permissions, the evaluator should resolve
	// using the page's resourceContext.SpacePerms list (the inherited
	// space-level grants).
	pageCtx := resourceContext{
		Type:    ResourcePage,
		Private: false,
		SpacePerms: []Permission{
			{SubjectType: "member", SubjectID: "u-1", Access: AccessEdit},
		},
	}
	got := resolveAccess(pageCtx, "u-1", nil)
	if got != AccessEdit {
		t.Fatalf("page should inherit from space, got %q", got)
	}
}

func TestPagePermissionsOverrideSpace(t *testing.T) {
	pageCtx := resourceContext{
		Type:    ResourcePage,
		Private: false,
		SpacePerms: []Permission{
			{SubjectType: "member", SubjectID: "u-1", Access: AccessView},
		},
	}
	pagePerms := []Permission{
		{SubjectType: "member", SubjectID: "u-1", Access: AccessAdmin},
	}
	got := resolveAccess(pageCtx, "u-1", pagePerms)
	if got != AccessAdmin {
		t.Fatalf("page-level perms must override space, got %q", got)
	}
}
