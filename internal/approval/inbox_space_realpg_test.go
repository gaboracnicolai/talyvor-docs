package approval_test

// THE APPROVAL INBOX COULD NOT ADDRESS THE PAGE IT WAS ABOUT.
//
// `GET /v1/workspaces/{wsID}/approvals/pending` answers with the approval_requests rows, and
// the SPA's inbox turns each into an "Open" button. A page's address in this product is
// `/spaces/{spaceID}/pages/{pageID}` (frontend/src/router/paths.ts) — and the response carried
// page_id and NO space at all. The inbox filled the hole with a hardcoded empty string, so the
// button navigated to a URL the app's route table resolves to its catch-all: Not found.
// (Measured with matchRoutes against the real route table; see
// frontend/src/router/approval-open.test.tsx for that half.)
//
// WHY THIS IS THE SERVER'S HALF AND NOT A UI PATCH: the space is not something the browser can
// derive. It is a column on the page row — `space_id TEXT NOT NULL REFERENCES spaces(id) ON
// DELETE CASCADE` (migrations/0002_pages.sql:13) — and approval_requests.page_id is itself
// `NOT NULL REFERENCES pages(id) ON DELETE CASCADE` (0009_approvals.sql:21). So every pending
// row has exactly one live page, and every page has exactly one space: the JOIN this test
// pins can neither drop a row nor invent one.
//
// WHAT IT ASSERTS AND WHY IN THIS SHAPE. It reads the HTTP RESPONSE BODY as generic JSON
// (map[string]any), not through the Go struct, because the defect is a field that never
// reached the wire — decoding into the type that has the field would make its presence
// unfalsifiable. The expected space is read with SQL against the pool, never through a Store
// getter: an oracle sharing a code path with its subject is not an independent oracle.
//
// RED FIRST: on 41367df this file fails with `space_id ABSENT from the pending row`.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/approval"
	"github.com/talyvor/docs/internal/testutil"
)

// pendingRows drives the shipped chain (gatewayauth → authz → the approval handler) and
// returns the response's rows as generic JSON.
func pendingRows(t *testing.T, chain http.Handler, ws, email string) []map[string]any {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/approvals/pending", nil)
	r.Header.Set("X-Gateway-Auth", apSecret)
	r.Header.Set("X-User-Email", email)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("pending = %d, want 200. body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var out []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pending body %q: %v", strings.TrimSpace(rr.Body.String()), err)
	}
	return out
}

func TestPending_CarriesTheSpaceThePageLivesIn_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	pageID := d.Page(t, ws, alice, "Runbook")

	// The oracle: the page's space, read with SQL against the pool.
	var wantSpace string
	if err := d.Pool.QueryRow(ctx,
		`SELECT space_id FROM pages WHERE id = $1`, pageID).Scan(&wantSpace); err != nil {
		t.Fatalf("read space_id: %v", err)
	}
	// PRECONDITION, ASSERTED: a blank oracle would make the comparison below meaningless.
	if wantSpace == "" {
		t.Fatalf("fixture is wrong: the seeded page has an empty space_id")
	}

	store := approval.NewStore(d.Pool)
	if _, err := store.RequestApproval(ctx, pageID, ws, alice, []string{bob}, "please", nil); err != nil {
		t.Fatalf("seed approval: %v", err)
	}

	rows := pendingRows(t, publishChain(d), ws, "bob@corp.com")
	// PRECONDITION: exactly the seeded row arrived. An empty list would pass every assertion
	// below by vacuity.
	if len(rows) != 1 {
		t.Fatalf("pending returned %d rows, want 1: %+v", len(rows), rows)
	}
	if got, _ := rows[0]["page_id"].(string); got != pageID {
		t.Fatalf("pending row page_id = %q, want %q", got, pageID)
	}

	got, ok := rows[0]["space_id"]
	if !ok {
		t.Fatalf("space_id ABSENT from the pending row — the inbox cannot address the page "+
			"this request is about, so its Open button navigates to /spaces//pages/%s, which "+
			"the SPA's route table resolves to its catch-all. row=%+v", pageID, rows[0])
	}
	if s, _ := got.(string); s != wantSpace {
		t.Fatalf("pending row space_id = %#v, want %q (the page's own space)", got, wantSpace)
	}
}

// A SECOND PAGE IN A SECOND SPACE. With one page in the fixture, a handler that reported ANY
// space — the workspace's first, a constant, the caller's — is indistinguishable from one that
// reports the row's own. This is the control that makes the assertion above mean what it says.
func TestPending_ReportsEachRowsOwnSpace_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")

	store := approval.NewStore(d.Pool)
	want := map[string]string{} // pageID → its space
	for _, title := range []string{"Runbook", "Postmortem"} {
		pageID := d.Page(t, ws, alice, title)
		var sp string
		if err := d.Pool.QueryRow(ctx,
			`SELECT space_id FROM pages WHERE id = $1`, pageID).Scan(&sp); err != nil {
			t.Fatalf("read space_id: %v", err)
		}
		want[pageID] = sp
		if _, err := store.RequestApproval(ctx, pageID, ws, alice, []string{bob}, "please", nil); err != nil {
			t.Fatalf("seed approval: %v", err)
		}
	}
	// PRECONDITION: testutil.Page mints a space per page, so the two spaces DIFFER. If it ever
	// stops doing that this control is worthless, and it must say so rather than pass.
	if len(want) != 2 {
		t.Fatalf("fixture: want 2 pages, got %d", len(want))
	}
	seen := map[string]bool{}
	for _, sp := range want {
		seen[sp] = true
	}
	if len(seen) != 2 {
		t.Fatalf("fixture: the two pages share a space (%v) — this control cannot distinguish "+
			"per-row from per-workspace reporting", want)
	}

	rows := pendingRows(t, publishChain(d), ws, "bob@corp.com")
	if len(rows) != 2 {
		t.Fatalf("pending returned %d rows, want 2: %+v", len(rows), rows)
	}
	for _, row := range rows {
		pageID, _ := row["page_id"].(string)
		space, _ := row["space_id"].(string)
		if space != want[pageID] {
			t.Fatalf("page %s reported space %q, want %q — the response is not per-row",
				pageID, space, want[pageID])
		}
	}
}

// WHICH HALF OF THE OLD COMMENT WAS TRUE, MEASURED RATHER THAN ARGUED.
//
// ApprovalInbox.tsx justified its empty space id with two claims: (i) "the server's decide
// endpoint doesn't actually need the spaceID (it's URL-decorative)" and (ii) "the page route is
// reconstructed by PageView once the user navigates in". (ii) is false — the route never
// matches, so PageView never mounts. (i) is TRUE, and this is the measurement: chi matches an
// empty {spaceID} segment and the route's enforcer resolves the page from {pageID}, so the
// inline Approve/Reject buttons worked while Open did not.
//
// ⚠ THIS IS A RECORD OF A MEASUREMENT, NOT A CONTRACT. Nothing in the product relies on it any
// more (the inbox now sends the real space). It exists so the next person does not have to
// re-derive why the fix is scoped to navigation, and so a router change that quietly starts
// rejecting these URLs is attributed here instead of being read as a new defect elsewhere.
//
// ⚠ AND A WRONG PREDICTION IS WHAT GAVE IT ITS CATCHER. I predicted this test's earning
// control would be a regex-bounded route param — `{spaceID:[^/]+}` instead of `{spaceID}` —
// and IT IS INERT: measured on all three patterns, `{spaceID}`, `{spaceID:[^/]+}` and
// `{spaceID:[a-zA-Z0-9-]+}` ALL answer 200 with spaceID="" for /spaces//pages/p-1/x. chi
// matches the param node and never applies the constraint to an empty segment, so anyone
// "bounding" one of these routes with a regex has changed nothing. The control that DOES earn
// this test is a handler-level rejection (scripts/w31-inbox-space-controls.py C9b), which is
// also the change a later hardening pass would plausibly make.
func TestDecide_WithAnEmptySpaceSegment_StillReachesTheHandler_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	store := approval.NewStore(d.Pool)
	chain := publishChain(d)

	decide := func(space, page, reqID string) (int, string) {
		r := httptest.NewRequest(http.MethodPost,
			"/v1/spaces/"+space+"/pages/"+page+"/approval/"+reqID+"/decide",
			strings.NewReader(`{"decision":"approved"}`))
		r.Header.Set("X-Gateway-Auth", apSecret)
		r.Header.Set("X-User-Email", "bob@corp.com")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	seed := func(title string) (pageID, spaceID, reqID string) {
		pageID = d.Page(t, ws, alice, title)
		if err := d.Pool.QueryRow(ctx,
			`SELECT space_id FROM pages WHERE id = $1`, pageID).Scan(&spaceID); err != nil {
			t.Fatalf("read space_id: %v", err)
		}
		req, err := store.RequestApproval(ctx, pageID, ws, alice, []string{bob}, "please", nil)
		if err != nil {
			t.Fatalf("seed approval: %v", err)
		}
		return pageID, spaceID, req.ID
	}
	recorded := func(reqID string) int {
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM review_decisions WHERE request_id = $1 AND decision = 'approved'`,
			reqID).Scan(&n); err != nil {
			t.Fatalf("count decisions: %v", err)
		}
		return n
	}

	// POSITIVE CONTROL: the real space segment, so a 200 below cannot be read as "this chain
	// answers 200 to anything".
	pA, sA, rA := seed("A")
	if code, body := decide(sA, pA, rA); code != http.StatusOK {
		t.Fatalf("decide with the real space = %d, want 200. body=%s", code, body)
	}
	if n := recorded(rA); n != 1 {
		t.Fatalf("decide with the real space recorded %d approvals, want 1", n)
	}

	// THE MEASUREMENT: the URL the inbox used to send.
	pB, _, rB := seed("B")
	code, body := decide("", pB, rB)
	if code != http.StatusOK {
		t.Fatalf("decide with an EMPTY space segment = %d, want 200 — the route no longer "+
			"tolerates it. body=%s", code, body)
	}
	if n := recorded(rB); n != 1 {
		t.Fatalf("decide with an empty space segment recorded %d approvals, want 1 — it "+
			"answered 200 without doing the work", n)
	}
}
