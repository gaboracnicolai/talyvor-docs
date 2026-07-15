package approval_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/approval"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/testutil"
)

// approval.Pending had two problems in one route:
//
//	r.Get("/workspaces/{wsID}/approvals/pending", h.Pending)   // NO .With(...) — ungated
//	reviewerID := r.URL.Query().Get("reviewer_id")             // client picks the reviewer
//	if reviewerID == "" { reviewerID = authz.ActorOrEmpty(...) }
//
// The query string PREFERS the client's value, so any member reads another member's
// pending-approval queue with ?reviewer_id=victim. It is workspace-scoped
// (a.workspace_id = ANY(caller's set)) so it is not cross-tenant — but it is a
// cross-member read of "what is this person being asked to review", inside the tenant.
//
// The {wsID} in the path was also ignored entirely: the handler passed the caller's FULL
// workspace set, so the route promised a per-workspace view it did not deliver.
//
// GREEN: {wsID} is authorized against the verified memberships, the reviewer is ALWAYS
// the caller's member id IN that workspace, and the results are scoped to it.

const apSecret = "sec4-test-gateway-secret-0123456789"

func apChain(d *testutil.DB) http.Handler {
	h := approval.NewHandler(approval.NewStore(d.Pool))
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(apSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func apReq(path, email string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Gateway-Auth", apSecret)
	r.Header.Set("X-User-Email", email)
	return r
}

func TestSec_ApprovalPending_ReviewerIsVerifiedNotQuery(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	bob := d.Member(t, ws, "bob@corp.com")
	d.Member(t, ws, "mallory@corp.com")
	pageID := d.Page(t, ws, alice, "Sensitive spec")

	// Bob is asked to review a page. This is Bob's queue.
	store := approval.NewStore(d.Pool)
	if _, err := store.RequestApproval(ctx, pageID, ws, alice, []string{bob}, "please review", nil); err != nil {
		t.Fatalf("seed approval request: %v", err)
	}

	chain := apChain(d)
	do := func(r *http.Request) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}
	base := "/v1/workspaces/" + ws + "/approvals/pending"

	// ANCHOR: Bob sees his OWN queue — proves the fixture is real, so an empty result for
	// Mallory below means "denied", not "nothing to find".
	rr := do(apReq(base, "bob@corp.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("Bob's own queue = %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), pageID) {
		t.Fatalf("Bob's own queue does not contain the seeded request — fixture is wrong. body=%s", rr.Body.String())
	}

	// THE READ: Mallory asks for Bob's queue by naming him.
	rr = do(apReq(base+"?reviewer_id="+bob, "mallory@corp.com"))
	if strings.Contains(rr.Body.String(), pageID) {
		t.Errorf("CROSS-MEMBER READ: Mallory read Bob's pending-approval queue via "+
			"?reviewer_id=<bob> (HTTP %d) — the query string PREFERRED the client's value over "+
			"the verified actor. body=%s", rr.Code, rr.Body.String())
	}

	// Mallory's own queue is legitimately empty (nobody asked her to review).
	rr = do(apReq(base, "mallory@corp.com"))
	if rr.Code != http.StatusOK {
		t.Errorf("Mallory's own queue = %d, want 200 (scope, not blanket-deny)", rr.Code)
	}
	if strings.Contains(rr.Body.String(), pageID) {
		t.Errorf("Mallory's own queue leaked Bob's item: %s", rr.Body.String())
	}

	// The route must be GATED: a verified caller who is not a member of {wsID} is denied
	// rather than silently served from their own workspace set.
	other := d.Workspace(t)
	d.Member(t, other, "outsider@corp.com")
	if rr := do(apReq(base, "outsider@corp.com")); rr.Code != http.StatusForbidden {
		t.Errorf("non-member of {wsID} = %d, want 403 (the route was ungated and ignored {wsID})", rr.Code)
	}
}
