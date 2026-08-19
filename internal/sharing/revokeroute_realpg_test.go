package sharing_test

// THE REVOKE ROUTE — THE ONE THAT TURNS OFF PUBLIC ACCESS — HAD NEVER BEEN EXECUTED, AND THE ENTRY
// THAT SAID IT HAD WAS TALKING ABOUT A DIFFERENT FUNCTION WITH THE SAME NAME.
//
// ⚠⚠ MEASURED, NOT ASSUMED. publiclane_realpg_test.go's own header lists the four zero-coverage
// functions it exists to close, and names this one:
//
//	internal/sharing/handler.go:143  Revoke  0.0%
//
// Re-deriving the census at THAT FILE'S OWN MERGE SHA (`74effc8a`) — `go test -coverprofile
// -coverpkg=./... ./...`, `go tool cover -func | awk '$3=="0.0%"'` — returns:
//
//	handler.go:62  MountPublic  100.0%
//	handler.go:72  writeErr     100.0%
//	handler.go:161 Public        77.8%
//	handler.go:143 Revoke         0.0%   ← still
//	store.go:215   Revoke        75.0%   ← this is the one that got covered
//
// Three of the four closed. The fourth did not: `TestPublicLane_RevokeActuallyRevokes_RealPG` and
// `TestPublicLane_RevokeIsScopedToItsPage_RealPG` both call `f.store.Revoke(...)` DIRECTLY. They are
// good tests of the STORE — and the HTTP route's chi param plumbing, its ErrShareLinkNotFound → 404
// mapping, its 500 arm and its `{"ok":true}` body were never run by anything.
//
// ⚠ THIS IS NOT A CRITICISM OF THAT MERGE, WHICH IS A GOOD ONE. It is the exact failure a coverage
// census exists to catch: a name that appears twice in one package, covered on the side nobody was
// asking about. It is visible only because the census was RE-RUN rather than read off a handover.
//
// ⚠ WHAT THE MEASUREMENT FOUND ABOUT THE PRODUCT: nothing wrong. Driven end to end through the real
// chain (chi /v1 + gatewayauth + authz + pageEnf) on real Postgres, all six axes below behave
// correctly on the unmodified tree. **RED-FIRST IS THEREFORE NOT CLAIMED HERE** — every assertion
// passes at `a641d4eb`, and each is earned by a positive control instead
// (scripts/w31-revoke-route-controls-9d47.py).
//
// ⚠ THE ONE THING THAT MAKES THIS MORE THAN COVERAGE: [REVOKE-TWICE] pins that a second revoke
// answers 404 and NOT `{"ok":true}`. That is the revoke-that-does-not-revoke lie — a route that
// reports success for a delete that removed nothing — and it is the shape a status-code test cannot
// see. The store's `RowsAffected() == 0 → ErrShareLinkNotFound` is what prevents it, and until now
// nothing connected that to the status a caller actually receives.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// revokeFixture reuses publicLaneChain — the same real router the rest of this file drives — and
// adds an AUTHENTICATED caller, which the public-lane tests deliberately never build: every request
// there is header-less by design.
type revokeFixture struct {
	*publicLaneFixture
	d       *testutil.DB
	spaceID string
}

func newRevokeFixture(t *testing.T) *revokeFixture {
	t.Helper()
	d := testutil.New(t)
	f := newPublicLaneFixture(t, d)
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id = $1`, f.pageID).Scan(&spaceID); err != nil {
		t.Fatalf("[SETUP] read the page's space: %v", err)
	}
	return &revokeFixture{publicLaneFixture: f, d: d, spaceID: spaceID}
}

// as drives the chain AS a gateway-verified caller: the transit proof plus the email claim, exactly
// the two headers cmd/docs/main.go's gateway supplies. Never a member id — authz resolves that from
// workspace_members, and handing it in would be testing a router that does not exist.
func (f *revokeFixture) as(t *testing.T, email, method, url string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	req.Header.Set("X-Gateway-Auth", publicLaneSecret)
	req.Header.Set("X-User-Email", email)
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func (f *revokeFixture) revokeURL(pageID, linkID string) string {
	return "/v1/spaces/" + f.spaceID + "/pages/" + pageID + "/share/" + linkID
}

// linkRowCount reads Postgres directly — never ListByPage, which the same edit could move.
func (f *revokeFixture) linkRowCount(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := f.d.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM share_links WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("count share_links %q: %v", id, err)
	}
	return n
}

// TestRevokeRoute_TurnsOffPublicAccess_RealPG is the route half of "a revoked link stops opening".
func TestRevokeRoute_TurnsOffPublicAccess_RealPG(t *testing.T) {
	f := newRevokeFixture(t)
	l := f.link(t, nil, "")

	// ── LIVENESS FLOOR. "the token 404s after the revoke" is satisfied by a token that never
	// worked. This fails the run before that can happen.
	if code, body := f.open(t, l.Token, ""); code != http.StatusOK {
		t.Fatalf("[LINK-LIVE-FIRST] the link does not open BEFORE the revoke (%d %s) — every "+
			"assertion below would pass on a link that was already dead", code, body)
	}

	code, body := f.as(t, "alice@example.com", http.MethodDelete, f.revokeURL(f.pageID, l.ID))
	if code != http.StatusOK {
		t.Fatalf("[REVOKE-SERVES] the admin's revoke of her own page's link answered %d: %s", code, body)
	}
	if body := body; body == "" || !containsOK(body) {
		t.Errorf("[REVOKE-SERVES] want an {\"ok\":true} body, got %q", body)
	}
	if n := f.linkRowCount(t, l.ID); n != 0 {
		t.Errorf("[REVOKE-SERVES] the route answered 200 and %d row(s) remain on disk", n)
	}

	// ── The product statement: the STRANGER's door is shut. Asserted through the public lane, not
	// through the row count above — a row deleted but a token still answering is the failure that
	// matters, and only the no-auth GET can see it.
	if code, body := f.open(t, l.Token, ""); code != http.StatusNotFound {
		t.Errorf("[REVOKED-TOKEN-DEAD] a revoked token still answers %d: %s", code, body)
	}

	// ── REVOKE-TWICE. THE revoke-that-does-not-revoke LIE. A second delete removed nothing, so it
	// must NOT report success. This is the arm a status-only test cannot distinguish.
	code, body = f.as(t, "alice@example.com", http.MethodDelete, f.revokeURL(f.pageID, l.ID))
	if code != http.StatusNotFound {
		t.Errorf("[REVOKE-TWICE] revoking an already-revoked link answered %d %s — a delete that "+
			"removed nothing reporting success is the lie this asserts against", code, body)
	}

	// ── An id that never existed is the same 404 — no existence oracle between "gone" and "never".
	code, body = f.as(t, "alice@example.com", http.MethodDelete,
		f.revokeURL(f.pageID, "3f1b9c2e-7a44-4d21-9f60-0c5e8a1b2d33"))
	if code != http.StatusNotFound {
		t.Errorf("[REVOKE-UNKNOWN] revoking an unknown id answered %d %s, want 404", code, body)
	}
}

// TestRevokeRoute_IsScopedToItsPage_RealPG pins the ce8bfe3 cross-page fix FROM THE ROUTE.
//
// ⚠ THE EXISTING GUARD FOR THIS CALLS THE STORE DIRECTLY, WHICH IS THE HALF THAT WAS COVERED. The
// route adds the part the store cannot state: {id} and {pageID} both come out of the URL, so a
// handler that read the wrong param — the exact defect fixed one package over in
// permission/crossresource_realpg_test.go, where one handler mounted twice named a resource its
// enforcer had not authorized — is invisible to a store-level test.
func TestRevokeRoute_IsScopedToItsPage_RealPG(t *testing.T) {
	f := newRevokeFixture(t)
	victim := f.link(t, nil, "")

	// A second page in the SAME workspace, authored by the same admin: the caller legitimately
	// admins both, so a refusal here is about the PAGE scope and nothing else.
	otherPage := f.d.Page(t, f.ws, f.author, "Alice's Other Doc")

	if code, _ := f.open(t, victim.Token, ""); code != http.StatusOK {
		t.Fatalf("[LINK-LIVE-FIRST] the victim link does not open before the attempt")
	}

	code, body := f.as(t, "alice@example.com", http.MethodDelete, f.revokeURL(otherPage, victim.ID))
	if code != http.StatusNotFound {
		t.Errorf("[REVOKE-SCOPED-TO-PAGE] revoking a link by naming a DIFFERENT page answered %d %s, "+
			"want 404", code, body)
	}
	if n := f.linkRowCount(t, victim.ID); n != 1 {
		t.Errorf("[REVOKE-SCOPED-TO-PAGE] the victim's link row is gone (%d rows) — a link was "+
			"revoked through a page that does not own it", n)
	}
	if code, body := f.open(t, victim.Token, ""); code != http.StatusOK {
		t.Errorf("[REVOKE-SCOPED-TO-PAGE] the victim's link stopped opening (%d %s) — the revoke "+
			"took effect through the wrong page", code, body)
	}
}

// TestRevokeRoute_RequiresAdminOnThePage_RealPG is the gate. Share links are public
// access-granting tokens; Mount gates all three sharing routes at AccessAdmin.
//
// ⚠ WITHOUT THIS, EVERY OTHER TEST IN THIS FILE IS SATISFIED BY AN UNGATED ROUTE. They all call as
// alice, who admins the page, so deleting `.With(h.pageEnf.Require(AccessAdmin))` leaves them green.
// Same pairing as TestPublicLane_ExemptionIsNarrow: one test says the door opens, this says it is a
// door.
func TestRevokeRoute_RequiresAdminOnThePage_RealPG(t *testing.T) {
	f := newRevokeFixture(t)
	l := f.link(t, nil, "")

	// ── LIVENESS FLOOR. ⚠ ADDED BECAUSE CONTROL C6 FOUND IT MISSING, not because anyone read for
	// it. This test ends by asserting the link STILL OPENS after the refused revoke — which a link
	// that never opened satisfies perfectly. C6 (the public lane forced to 404) fired that closing
	// assertion under the [REVOKE-NEEDS-ADMIN] tag, i.e. the gate assertion went red for a reason
	// that had nothing to do with the gate. With the floor here, that mutation stops the test where
	// it belongs and the tag means only what it says.
	if code, body := f.open(t, l.Token, ""); code != http.StatusOK {
		t.Fatalf("[LINK-LIVE-FIRST] the link does not open BEFORE the refused revoke (%d %s)", code, body)
	}

	// bob is a plain member of the same workspace. The space is PUBLIC, so resolveAccess's
	// default gives him `view` — a real, non-zero access level that is below Admin. That is the
	// interesting caller: not a stranger, a colleague.
	f.d.Member(t, f.ws, "bob@example.com")

	code, body := f.as(t, "bob@example.com", http.MethodDelete, f.revokeURL(f.pageID, l.ID))
	if code != http.StatusForbidden {
		t.Errorf("[REVOKE-NEEDS-ADMIN] a view-tier member revoked (or was refused with the wrong "+
			"status): %d %s, want 403", code, body)
	}
	if n := f.linkRowCount(t, l.ID); n != 1 {
		t.Errorf("[REVOKE-NEEDS-ADMIN] the link row is gone (%d rows) — a non-admin's revoke landed", n)
	}
	if code, _ := f.open(t, l.Token, ""); code != http.StatusOK {
		t.Errorf("[REVOKE-NEEDS-ADMIN] the link stopped opening after a refused revoke (%d)", code)
	}
}

// TestRevokeRoute_CrossTenantIsNotFound_RealPG: a caller with no membership in the page's workspace
// gets 404, never 403 — the no-oracle convention RequireAccess uses, asserted at this route.
func TestRevokeRoute_CrossTenantIsNotFound_RealPG(t *testing.T) {
	f := newRevokeFixture(t)
	l := f.link(t, nil, "")

	// mallory belongs to a DIFFERENT workspace entirely.
	otherWS := f.d.Workspace(t)
	f.d.Member(t, otherWS, "mallory@example.com")

	code, body := f.as(t, "mallory@example.com", http.MethodDelete, f.revokeURL(f.pageID, l.ID))
	if code != http.StatusNotFound {
		t.Errorf("[REVOKE-CROSS-TENANT] a foreign tenant got %d %s, want 404 (403 would confirm the "+
			"page exists)", code, body)
	}
	if n := f.linkRowCount(t, l.ID); n != 1 {
		t.Errorf("[REVOKE-CROSS-TENANT] the link row is gone (%d rows) — a cross-tenant revoke landed", n)
	}
}

// containsOK is a deliberate substring check rather than a JSON decode: the assertion is about the
// literal body a client receives, and decoding into a struct would accept `{}` for a missing field.
func containsOK(body string) bool {
	for i := 0; i+len(`"ok":true`) <= len(body); i++ {
		if body[i:i+len(`"ok":true`)] == `"ok":true` {
			return true
		}
	}
	return false
}
