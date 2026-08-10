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
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// AN APPROVAL DECISION IS ANSWERED BY WHATEVER PAGE THE CALLER NAMES, NOT BY THE PAGE THE REQUEST
// IS ON — FINDING (18)'s FOURTH COPY, AND THE FIRST ONE FOUND BY A GUARD RATHER THAN BY READING.
//
//	POST /spaces/{spaceID}/pages/{pageID}/approval/{requestID}/decide     View
//
// pageEnf resolves {pageID} and answers about THAT page. `Decide` then reads only {requestID} and
// hands it to the store, which gates on `SELECT EXISTS(… approval_requests WHERE id = $1 AND
// workspace_id = ANY($2))`. So the gate authorizes one object and the statement acts on another,
// with only the WORKSPACE — the ring both are already inside — between them. Its own sibling
// `Publish` on the very next line reads chi.URLParam(r, "pageID"); `Decide` is the outlier.
//
// ⚠ THE STORE'S DOC COMMENT ARGUES FOR THE WRONG RING, IN DETAIL, EXACTLY LIKE #83's DID:
// "the request must live in one of the caller's workspaces before we touch any decision row …
// Once that check passes, the subsequent bare-id statements are safe: the request is confirmed
// in-workspace and its page_id is owned by the same request." Every clause is true. The ring it
// closes is the cross-tenant one, which was never the open one.
//
// ⚠ WHAT MAKES THIS ONE DIFFERENT FROM THE FIRST THREE: the caller here is an ASSIGNED REVIEWER.
// The `UPDATE review_decisions … WHERE request_id = $3 AND reviewer_id = $4` means a stranger's
// decision matches no row, so this is not "anyone can decide anything". It is narrower and
// stranger than that: the PRODUCT'S OWN POSITION is that bob may not act on this page — it
// refuses him at the request's own address — and the borrowed address overrides that refusal and
// flips a document he cannot read from draft to approved.
//
// MEASURED THROUGH THE REAL ROUTES ON REAL POSTGRES BEFORE ANY CHANGE (see the queue entry for
// the numbers). The fixture is this package's own; page status is read with SQL against the pool
// rather than through a store getter, because an oracle that shares a code path with its subject
// is not an independent oracle.
func TestApprovalDecide_MustBelongToTheAuthorizedPage_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	bob := d.Member(t, ws, "bob@example.com")

	// A PRIVATE space alice owns. bob has no grant, so resolveAccess gives him AccessNone — he
	// cannot read the page at all.
	privSpace := seedSpaceA(t, d, ws, alice, "Board Private", true)
	privPage := seedPageA(t, d, ws, privSpace, alice, "Board Memo")
	// bob's OWN space. He created it, so owner-is-admin gives him Admin on his own page. Nobody
	// granted him anything, and `POST /spaces` is registered with no enforcer at all — there is
	// no privileged step in the chain.
	bobSpace := seedSpaceA(t, d, ws, bob, "Bobs Own", false)
	bobPage := seedPageA(t, d, ws, bobSpace, bob, "Bobs Page")

	store := approval.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)

	// alice asks bob to review her PRIVATE page. This is an ordinary product action and it is the
	// whole premise: it is what puts bob's member id in review_decisions for a page he cannot open.
	req, err := store.RequestApproval(ctx, privPage, ws, alice, []string{bob}, "please review", nil)
	if err != nil {
		t.Fatalf("seed approval request: %v", err)
	}

	// The looker and the enforcer are cmd/docs/main.go's, re-derived here rather than borrowed
	// from another package's test — a helper lends the evidence of the test it was written for.
	// Wiring the REAL enforcer is what makes "the test skipped WithAccess" unavailable as an
	// explanation for anything below.
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		pg, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, pg.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID,
			SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))

	do := func(email, memberID, method, path, body string) (int, string) {
		t.Helper()
		h := approval.NewHandler(store).WithAccess(pageEnf)
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				c := authz.WithMemberships(rq.Context(), email,
					[]authz.Membership{{WorkspaceID: ws, MemberID: memberID}})
				next.ServeHTTP(w, rq.WithContext(c))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		rq := httptest.NewRequest(method, path, strings.NewReader(body))
		rq.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, rq)
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}
	decideURL := func(spaceID, pageID, requestID string) string {
		return "/v1/spaces/" + spaceID + "/pages/" + pageID + "/approval/" + requestID + "/decide"
	}
	// Read the truth with SQL, not through a store getter.
	pageStatus := func(t *testing.T, pageID string) string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx, `SELECT doc_status FROM pages WHERE id = $1`, pageID).Scan(&s); err != nil {
			t.Fatalf("read page status: %v", err)
		}
		return s
	}
	decisionOf := func(t *testing.T, requestID, reviewer string) string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx,
			`SELECT decision FROM review_decisions WHERE request_id = $1 AND reviewer_id = $2`,
			requestID, reviewer).Scan(&s); err != nil {
			t.Fatalf("read decision: %v", err)
		}
		return s
	}
	reqStatus := func(t *testing.T, requestID string) string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx,
			`SELECT status FROM approval_requests WHERE id = $1`, requestID).Scan(&s); err != nil {
			t.Fatalf("read request status: %v", err)
		}
		return s
	}

	// ── A-PREMISE. An instrument check. bob's OWN page must answer this route at all, or every
	// refusal below is an absence of a working route rather than a scope decision. It uses a
	// request of bob's own so it can never be the leak assertion in disguise.
	ownReq, err := store.RequestApproval(ctx, bobPage, ws, bob, []string{bob}, "self", nil)
	if err != nil {
		t.Fatalf("seed bob's own request: %v", err)
	}
	if code, body := do("bob@example.com", bob, http.MethodPost,
		decideURL(bobSpace, bobPage, ownReq.ID), `{"decision":"approved"}`); code != http.StatusOK {
		t.Fatalf("[A-PREMISE] PREMISE FAILED: bob cannot decide his OWN page's request through its "+
			"own address (%d %s) — the route is not serving", code, body)
	}

	// ── A-HONEST. The gate itself, at the request's OWN address. This is what makes the next
	// assertion a statement about the PAIR rather than about bob's access: the product already
	// knows bob may not act on this page, even though he is a named reviewer on it.
	if code, _ := do("bob@example.com", bob, http.MethodPost,
		decideURL(privSpace, privPage, req.ID), `{"decision":"approved"}`); code == http.StatusOK {
		t.Errorf("[A-HONEST] bob decided the PRIVATE page's request at its OWN address (200) — the " +
			"page enforcer is not refusing, so anything below is a broken gate rather than a " +
			"mismatched pair, and the fix belongs somewhere else")
	}
	if got := decisionOf(t, req.ID, bob); got != "pending" {
		t.Fatalf("[A-HONEST] the honest-address call already moved the decision to %q — the leak "+
			"assertion below would be reading this call's write, not its own", got)
	}

	// ── A-LEAK-DECIDE. THE DEFECT. Same reviewer, same request, a borrowed page in the URL.
	code, body := do("bob@example.com", bob, http.MethodPost,
		decideURL(bobSpace, bobPage, req.ID), `{"decision":"approved"}`)
	if code != http.StatusNotFound {
		if code == http.StatusOK {
			t.Errorf("[A-LEAK-DECIDE] LEAK: bob has NO grant on the private space and is REFUSED at "+
				"the request's own address, yet recorded a decision on it by naming a page of his "+
				"OWN in the URL: %s", body)
		} else {
			t.Errorf("[A-LEAK-DECIDE] a mismatched (page, request) pair answered %d, want 404 — 404 "+
				"is the no-oracle answer (403 would confirm the id exists somewhere the caller "+
				"cannot reach): %s", code, body)
		}
	}

	// ── A-LEAK-DECISION-ROW. The write, read back independently. Without this, a route that
	// returned 200 and did nothing would satisfy the assertion above and read as a leak.
	if got := decisionOf(t, req.ID, bob); got != "pending" {
		t.Errorf("[A-LEAK-DECISION-ROW] the decision row on the private page's request is now %q — "+
			"written through an address that authorized a different page", got)
	}

	// ── A-LEAK-PAGE-STATUS. THE CONSEQUENCE, and the reason this is not merely untidy: bob was
	// the only reviewer, so his decision is FINAL and the store flips the request AND the page in
	// lockstep. A document in a space he cannot open changes state because he named his own page.
	if got := reqStatus(t, req.ID); got != "pending" {
		t.Errorf("[A-LEAK-PAGE-STATUS] the PRIVATE page's approval request is now %q — a workflow "+
			"state on a page the caller cannot read, driven from a borrowed address", got)
	}
	if got := pageStatus(t, privPage); got == "approved" {
		t.Errorf("[A-LEAK-PAGE-STATUS] the PRIVATE page itself is now %q — bob cannot open it and "+
			"he published its approval state", got)
	}
}

func seedSpaceA(t *testing.T, d *testutil.DB, wsID, creator, name string, private bool) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO spaces (workspace_id, name, slug, created_by, private)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		wsID, name, "spa-"+name+"-"+wsID, creator, private).Scan(&id); err != nil {
		t.Fatalf("seed space %q: %v", name, err)
	}
	return id
}

func seedPageA(t *testing.T, d *testutil.DB, wsID, spaceID, author, title string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		spaceID, wsID, title, "pga-"+title+"-"+spaceID, author, "body", []byte(`{}`)).Scan(&id); err != nil {
		t.Fatalf("seed page %q: %v", title, err)
	}
	return id
}
