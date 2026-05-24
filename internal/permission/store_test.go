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
	return newStore(pool), pool
}

func TestGrant_UpsertsPermissionRow(t *testing.T) {
	store, pool := newMockStore(t)

	pool.ExpectExec(`INSERT INTO permissions`).
		WithArgs("space", "sp-1", "member", "u-1", "edit", "ws-1", "u-admin").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := store.Grant(context.Background(), Permission{
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
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestGrant_RejectsInvalidAccess(t *testing.T) {
	store, _ := newMockStore(t)
	err := store.Grant(context.Background(), Permission{
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

func TestListForResource_ReturnsAllGrants(t *testing.T) {
	store, pool := newMockStore(t)
	now := time.Now().UTC()

	pool.ExpectQuery(`SELECT.*FROM permissions WHERE resource_type`).
		WithArgs("space", "sp-1").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "resource_type", "resource_id", "subject_type", "subject_id",
			"access", "workspace_id", "granted_by", "created_at",
		}).
			AddRow("p-1", "space", "sp-1", "member", "u-1", "edit", "ws-1", "u-admin", now).
			AddRow("p-2", "space", "sp-1", "team", "t-1", "view", "ws-1", "u-admin", now))

	out, err := store.ListForResource(context.Background(), ResourceSpace, "sp-1")
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
