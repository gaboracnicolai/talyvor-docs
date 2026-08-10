package customdomain

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// THE FOUR `pool.ExpectationsWereMet()` CALLS THAT USED TO END EVERY TEST IN THIS FILE ARE NOW
// ON `newMockStore` — MOVED, NOT DROPPED. Read the comment there for why; the short version is
// that this file asked the question per test and store_test.go's nine expectations never asked at
// all, which is exactly the split that lets the next test written be uncovered. The claim each
// call carried is unchanged and still holds for these four: that `Create` refuses BEFORE it
// reaches the COUNT and the INSERT (an unconsumed expectation is what says so), and that a nil
// space performs no space lookup.
func nowT() time.Time { return time.Now().UTC() }

func expectSpaceLookup(pool pgxmock.PgxPoolIface, wsID string, private bool) {
	pool.ExpectQuery(`SELECT.*FROM spaces WHERE id`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"workspace_id", "private"}).AddRow(wsID, private))
}

// A custom domain maps to a space, and the public renderer then serves EVERY page in that
// space with no authentication: publicIndex lists whatever ListBySpace returns and publicPage
// serves any slug via GetBySlug, whose query is `WHERE space_id = $1 AND slug = $2` — no
// privacy predicate, and model.Page has no published/visibility field to apply one to.
//
// space_id arrives in the request BODY and was written to the row unvalidated. Two
// consequences, both closed here:
//
//  1. CROSS-TENANT. A tenant could point their own DNS-verified domain at ANOTHER
//     workspace's space id and publish that tenant's pages. The wsID path param is
//     authorized (handler.go), but the body's space_id never was.
//  2. SILENT PUBLICATION OF A PRIVATE SPACE. Mapping a domain to a space marked Private
//     world-published it with no warning and no per-page control.
//
// This is the cheap half of the fix: refuse the mapping. The full fix — a per-page
// `published` column defaulting to false — needs a migration and stays scoped.
func TestCreate_RefusesForeignWorkspaceSpace(t *testing.T) {
	st, pool := newMockStore(t, &fakeResolver{})
	// The space belongs to ws-victim; the caller is creating in ws-attacker.
	expectSpaceLookup(pool, "ws-victim", false)

	_, err := st.Create(context.Background(), "ws-attacker", "docs.attacker.example", "m-1", ptrStr("sp-victim"))
	if err == nil {
		t.Fatal("mapping a domain to ANOTHER workspace's space must be refused — the public " +
			"renderer would publish that tenant's pages unauthenticated")
	}
	if !strings.Contains(err.Error(), "space") {
		t.Errorf("error should name the space as the problem, got %v", err)
	}
}

func TestCreate_RefusesPrivateSpace(t *testing.T) {
	st, pool := newMockStore(t, &fakeResolver{})
	// Same workspace, but the space is private.
	expectSpaceLookup(pool, "ws-1", true)

	_, err := st.Create(context.Background(), "ws-1", "docs.example.com", "m-1", ptrStr("sp-private"))
	if err == nil {
		t.Fatal("mapping a domain to a PRIVATE space must be refused: the public renderer has " +
			"no per-page filter, so this world-publishes every page in it silently")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Errorf("error should say the space is private so the operator can act on it, got %v", err)
	}
}

// POSITIVE CONTROL 1: a public space in the caller's own workspace still maps. Without this,
// "refuse everything" would pass the two tests above.
func TestCreate_AllowsPublicSpaceInOwnWorkspace(t *testing.T) {
	st, pool := newMockStore(t, &fakeResolver{})
	expectSpaceLookup(pool, "ws-1", false)
	pool.ExpectQuery(`SELECT COUNT.*FROM custom_domains WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int(0)))
	pool.ExpectQuery(`INSERT INTO custom_domains`).
		WithArgs("ws-1", "docs.example.com", ptrStr("sp-public"), pgxmock.AnyArg(), "m-1").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"cd-1", "ws-1", "docs.example.com", ptrStr("sp-public"), false,
			"talyvor-verify-x", "pending", "m-1", nowT(), nowT()))

	cd, err := st.Create(context.Background(), "ws-1", "docs.example.com", "m-1", ptrStr("sp-public"))
	if err != nil {
		t.Fatalf("a public space in the caller's own workspace must still map: %v", err)
	}
	if cd == nil || cd.SpaceID == nil || *cd.SpaceID != "sp-public" {
		t.Errorf("expected the mapping to be created for sp-public, got %+v", cd)
	}
}

// POSITIVE CONTROL 2: a domain with NO space mapping is still allowed — it renders the
// "no space configured" page, publishes nothing, and must not require a space lookup.
func TestCreate_AllowsNilSpace(t *testing.T) {
	st, pool := newMockStore(t, &fakeResolver{})
	pool.ExpectQuery(`SELECT COUNT.*FROM custom_domains WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int(0)))
	pool.ExpectQuery(`INSERT INTO custom_domains`).
		WithArgs("ws-1", "docs.example.com", (*string)(nil), pgxmock.AnyArg(), "m-1").
		WillReturnRows(pgxmock.NewRows(domainCols()).AddRow(
			"cd-1", "ws-1", "docs.example.com", (*string)(nil), false,
			"talyvor-verify-x", "pending", "m-1", nowT(), nowT()))

	if _, err := st.Create(context.Background(), "ws-1", "docs.example.com", "m-1", nil); err != nil {
		t.Fatalf("a domain with no space mapping must still be creatable: %v", err)
	}
}
