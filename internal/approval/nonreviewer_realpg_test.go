package approval_test

import (
	"context"
	"encoding/json"
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

// `POST …/approval/{requestID}/decide` ANSWERS 200 {"ok":true} TO A CALLER WHO IS NOT A REVIEWER,
// AND WRITES NOTHING — THE SUCCESS IS THE DEFECT, NOT THE WRITE.
//
// Decide's only reviewer test is the UPDATE's own WHERE clause:
//
//	UPDATE review_decisions SET decision = $1, comment = $2
//	 WHERE request_id = $3 AND reviewer_id = $4
//
// and its pgconn.CommandTag is discarded. A caller who is not on the request matches zero rows,
// falls through to aggregate(), and is told `{"ok":true}` with HTTP 200.
//
// ⚠ THE ZERO-ROW PROPERTY IS ALREADY WRITTEN DOWN IN THIS PACKAGE AS A SAFETY PROPERTY, WHICH IS
// HOW THE OTHER HALF STAYED OPEN. crosspage_realpg_test.go:38-42 says "a stranger's decision
// matches no row, so this is not 'anyone can decide anything'". That is true and it is the whole
// of what was ever measured: nothing anywhere asked what the stranger is TOLD. The write side is
// safe; the answer is a lie.
//
// ⚠ WHY IT IS REACHABLE BY DESIGN AND NOT ONLY BY MISUSE: the route is
// `.With(pageEnf.Require(AccessView))`, and resolveAccess grants AccessView to EVERY workspace
// member on a page in a NON-PRIVATE space with no explicit grant at all (store.go's "No explicit
// grant. Public space → workspace members get view"). So on the ordinary open space that most
// Docs content lives in, every colleague of the author passes the gate and gets `{"ok":true}` for
// a verdict the product never records.
//
// ⚠ AND THE CONSEQUENCE IS NOT COSMETIC. The reviewer inbox (`GET /workspaces/{wsID}/approvals/
// pending`) is driven by review_decisions rows, so the person who was told "ok" keeps the item in
// their queue forever, and the request stays `pending` with no signal that the click did nothing.
// A quorum workflow whose failure mode is indistinguishable from success cannot be operated.
//
// MEASURED THROUGH THE SHIPPED ROUTES ON REAL POSTGRES with the production enforcer wired. The
// decision rows are read back with SQL against the pool rather than through a store getter — an
// oracle that shares a code path with its subject is not an independent oracle.
func TestApprovalDecide_ANonReviewerIsNotToldItWorked_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com") // author
	carol := d.Member(t, ws, "carol@example.com") // the ONLY named reviewer
	bob := d.Member(t, ws, "bob@example.com")     // a colleague, on no request

	// A NON-PRIVATE space. bob has no grant of any kind; resolveAccess's public-space default is
	// what lets him past the route's AccessView gate, which is the point — this is the ordinary
	// configuration, not a misconfigured one.
	openSpace := seedSpaceA(t, d, ws, alice, "Handbook", false)
	pg := seedPageA(t, d, ws, openSpace, alice, "Expenses Policy")

	store := approval.NewStore(d.Pool)
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)

	req, err := store.RequestApproval(ctx, pg, ws, alice, []string{carol}, "please review", nil)
	if err != nil {
		t.Fatalf("seed approval request: %v", err)
	}

	// cmd/docs/main.go's looker + enforcer, re-derived here rather than borrowed from another
	// package's test: a helper lends the evidence of the test it was written for.
	pageLooker := func(ctx context.Context, id string) (permission.PageMeta, error) {
		p, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		sp, err := spaceStore.GetByIDInWorkspaces(ctx, p.SpaceID, authz.WorkspaceIDs(ctx))
		if err != nil {
			return permission.PageMeta{}, err
		}
		return permission.PageMeta{
			WorkspaceID: p.WorkspaceID, SpaceID: p.SpaceID,
			SpaceCreatedBy: sp.CreatedBy, SpacePrivate: sp.Private, PageCreatedBy: p.CreatedBy,
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
	base := "/v1/spaces/" + openSpace + "/pages/" + pg
	decideURL := base + "/approval/" + req.ID + "/decide"

	// Truth, read with SQL. countDecisions is the half that keeps [N-SILENT] from being
	// satisfiable by a route that 403s AND quietly stopped writing for everyone.
	decisionOf := func(t *testing.T, reviewer string) (string, bool) {
		t.Helper()
		var s string
		err := d.Pool.QueryRow(ctx,
			`SELECT decision FROM review_decisions WHERE request_id = $1 AND reviewer_id = $2`,
			req.ID, reviewer).Scan(&s)
		if err != nil {
			return "", false
		}
		return s, true
	}
	countDecisions := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := d.Pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM review_decisions WHERE request_id = $1`, req.ID).Scan(&n); err != nil {
			t.Fatalf("count decisions: %v", err)
		}
		return n
	}

	// ── N-SEEDED. The request seeded exactly the reviewers it was given, and bob is not among
	// them. Without this the whole test could be measuring a request that has no rows at all.
	if n := countDecisions(t); n != 1 {
		t.Fatalf("[N-SEEDED] PREMISE FAILED: the request seeded %d decision rows, want 1 (carol)", n)
	}
	if _, ok := decisionOf(t, bob); ok {
		t.Fatalf("[N-SEEDED] PREMISE FAILED: bob already has a decision row — he is a reviewer here, " +
			"so nothing below is about a non-reviewer")
	}

	// ── N-GATE. bob really is admitted by the route's enforcer. This is the instrument check: it
	// uses the sibling GET on the SAME gate (AccessView), so a refusal on the POST below can only
	// be Decide's own decision and never an access refusal in disguise. If Docs ever tightens the
	// public-space default, THIS is the line that reddens and says so.
	if code, body := do("bob@example.com", bob, http.MethodGet, base+"/approval", ""); code != http.StatusOK {
		t.Fatalf("[N-GATE] PREMISE FAILED: bob is refused (%d %s) by the AccessView gate this route "+
			"shares, so he never reaches Decide and this test measures nothing", code, body)
	}

	// ── N-SILENT. THE DEFECT. A caller on no decision row is told the verdict was recorded.
	code, body := do("bob@example.com", bob, http.MethodPost, decideURL, `{"decision":"approved"}`)
	if code == http.StatusOK {
		t.Errorf("[N-SILENT] bob is on NO decision row for this request and the route answered "+
			"200 %s — the UPDATE matched zero rows and its CommandTag is discarded, so a verdict "+
			"that was never recorded is reported as recorded", body)
	}
	if code != http.StatusForbidden {
		t.Errorf("[N-SILENT] want 403 (the request and its reviewer list are already readable by "+
			"this caller through GET …/approval, so refusing plainly discloses nothing new), got "+
			"%d: %s", code, body)
	}

	// ── N-NOWRITE. The write side, read back independently. Without it a route that started
	// CREATING a decision row for bob would satisfy [N-SILENT] by being a worse defect.
	if _, ok := decisionOf(t, bob); ok {
		t.Errorf("[N-NOWRITE] a decision row now exists for bob — the refusal must not be a row " +
			"that was written and then reported as refused")
	}
	if n := countDecisions(t); n != 1 {
		t.Errorf("[N-NOWRITE] the request now has %d decision rows, want 1 — the refused call "+
			"changed the reviewer set", n)
	}

	// ── N-REAL. THE ANTI-OVERFIX CONTROL, and it runs LAST so it cannot mask the assertions
	// above. The genuine reviewer must still be served: 200, her row moves, the request and the
	// page reach approved. A "fix" that refuses everyone reddens here.
	if code, body := do("carol@example.com", carol, http.MethodPost, decideURL,
		`{"decision":"approved"}`); code != http.StatusOK {
		t.Fatalf("[N-REAL] the NAMED reviewer was refused %d %s — the refusal is too wide", code, body)
	}
	if got, ok := decisionOf(t, carol); !ok || got != "approved" {
		t.Errorf("[N-REAL] carol's decision row is %q (present=%v), want \"approved\"", got, ok)
	}
	var reqStatus, docStatus string
	if err := d.Pool.QueryRow(ctx,
		`SELECT a.status, p.doc_status FROM approval_requests a
		   JOIN pages p ON p.id = a.page_id WHERE a.id = $1`, req.ID).Scan(&reqStatus, &docStatus); err != nil {
		t.Fatalf("read request/page status: %v", err)
	}
	if reqStatus != "approved" || docStatus != "approved" {
		t.Errorf("[N-REAL] after the only named reviewer approved, request=%q page=%q, want both "+
			"\"approved\" — the aggregate no longer completes", reqStatus, docStatus)
	}
}

// A REQUEST NAMING A REVIEWER WHO IS NOT A MEMBER OF THE WORKSPACE IS ACCEPTED, AND IS THEN
// UNAPPROVABLE FOREVER — MEASURED, NOT FIXED, BECAUSE THE FIX IS A PRODUCT DECISION.
//
// RequestApproval's only validation of `reviewers` is `len(reviewers) == 0`. Nothing checks the
// ids against workspace_members, against the page's readers, or against anything else. Each id
// gets a seeded `pending` row, and aggregate() requires `pending == 0` — so one unreachable id
// pins the request at pending for the life of the page, and the real reviewers who approve are
// told 200 each time.
//
// ⚠ THIS IS DELIBERATELY NOT FIXED HERE. Whether Docs may name a reviewer who is not yet a member
// (invite-then-review is a normal workflow) is a decision, and the queue's rule is to measure and
// report rather than to guess on it. What IS fixed in this commit is the half that is not a
// decision under any answer: a caller on no row must not be told the verdict landed.
func TestApprovalRequest_ReviewerIdsAreNotValidated_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	carol := d.Member(t, ws, "carol@example.com")

	sp := seedSpaceA(t, d, ws, alice, "Handbook NV", false)
	pg := seedPageA(t, d, ws, sp, alice, "Policy NV")
	store := approval.NewStore(d.Pool)

	// "mbr_typo" belongs to nobody: not a member of this workspace, not a member of any.
	req, err := store.RequestApproval(ctx, pg, ws, alice, []string{carol, "mbr_typo"}, "review", nil)
	if err != nil {
		t.Fatalf("[R-ACCEPTED] RequestApproval refused an unresolvable reviewer id (%v) — if Docs "+
			"has started validating reviewers, this test is the record of the old behaviour and "+
			"should be replaced by the validation's own guard", err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM review_decisions WHERE request_id = $1`, req.ID).Scan(&n); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if n != 2 {
		t.Fatalf("[R-ACCEPTED] seeded %d decision rows, want 2 — the premise of the pin below", n)
	}

	// The only reachable reviewer approves. The request cannot advance, because aggregate()
	// requires pending == 0 and "mbr_typo" can never be moved by any caller.
	if err := store.Decide(ctx, req.ID, pg, carol, "approved", "", []string{ws}); err != nil {
		t.Fatalf("carol decide: %v", err)
	}
	var status, docStatus string
	if err := d.Pool.QueryRow(ctx,
		`SELECT a.status, p.doc_status FROM approval_requests a
		   JOIN pages p ON p.id = a.page_id WHERE a.id = $1`, req.ID).Scan(&status, &docStatus); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "pending" || docStatus != "in_review" {
		t.Fatalf("[R-PINNED] request=%q page=%q — the unresolvable reviewer no longer pins the "+
			"request, so this recorded behaviour has changed and the finding needs re-measuring",
			status, docStatus)
	}
	// Say the number out loud in the log so a reader of a passing run still sees the finding.
	body, _ := json.Marshal(map[string]any{"reviewers": req.Reviewers, "request_status": status,
		"page_status": docStatus})
	t.Logf("[R-PINNED] MEASURED, NOT FIXED: %s — one unresolvable reviewer id pins the request at "+
		"pending permanently; reviewer ids are not validated at write time", body)
}
