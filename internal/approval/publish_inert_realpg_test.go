package approval_test

// PUBLISH IS A NO-OP AND THIS TEST PINS THAT AS A MEASURED FACT, NOT AS A SPECIFICATION.
//
// `POST /v1/spaces/{spaceID}/pages/{pageID}/publish` reaches Store.PublishApproved, which:
//
//	SELECT doc_status FROM pages WHERE id = $1 AND workspace_id = ANY($2)
//	if status != "approved" { return "page must be approved before publishing" }
//	UPDATE pages SET doc_status = $1 WHERE id = $2 AND workspace_id = ANY($3)   // $1 = "approved"
//
// The UPDATE writes the value the SELECT three lines above it has just REQUIRED the row to
// already hold. It is structurally incapable of changing doc_status — or, as measured below,
// any other byte of the row: it sets that one column and no timestamp. (Counted, not assumed:
// all four `UPDATE pages SET doc_status` sites in this package omit updated_at; the only
// UPDATE here that maintains one is the `approval_requests` write at store.go:254. So the
// missing timestamp is this package's convention and NOT a second defect.) There is no
// DocStatus named "published" (draft, in_review, approved,
// rejected, archived are the five, and migration 0009's own comment gives the lifecycle as
// "draft → in_review → approved | rejected"), no published_at on `pages`, and no reader
// anywhere that distinguishes a published page from an approved one.
//
// So the SPA's Publish CTA (ApprovalPanel.tsx:198-204, rendered in the `approved` branch at
// :57) posts, gets 200 {"ok":true}, invalidates, and re-renders the identical branch with the
// identical button. Clicking it once is indistinguishable from clicking it never.
//
// WHY THIS IS A TRIPWIRE AND NOT A CONTRACT. Whether publishing should mean something — a
// sixth lifecycle state, a published_at, or a UI that stops offering the button — is a
// product decision and is NOT made here. This test exists so that the day someone makes it,
// the change is DELIBERATE: it reds the moment publish starts altering the row.
//
// ⚠ WHAT IT CANNOT SEE, AND THIS IS THE POINT. W3.1 finding (11) asked for "a real-PG test
// reading the row with SQL against the pool, earned by a P-class mutation (` AND FALSE`, the
// statement runs and matches nothing)". FOR THIS WRITE THAT PRESCRIPTION PRODUCES A GUARD
// THAT CANNOT FAIL, and it is measured rather than argued — see
// scripts/w31-publish-inert-controls.py:
//
//	Q1  ` AND FALSE` appended to the UPDATE's WHERE  → NOT CAUGHT here (row was already right)
//	Q2  the UPDATE statement deleted outright        → NOT CAUGHT here (caught by the mock only)
//	Q3  the UPDATE writes DocDraft instead           → CAUGHT here, and by nothing else
//
// Q3 is the only mutation this test can see and is what earns it. A row assertion cannot
// detect an inert write disappearing, because there is nothing for it to have done. The
// four-instance class this item keeps finding — a guard that reads like protection and
// cannot fire — is what finding (11) would have added a fifth member to.
//
// ⚠ THIS TEST PASSES ON THE UNMODIFIED TREE BY CONSTRUCTION. It pins a status quo, so it has
// no red-first moment; Q3 is its entire justification.

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
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// publishChain mounts the approval routes the way cmd/docs/main.go does — gatewayauth, then
// authz, then a REAL permission.Enforcer over the page. The enforcer is not optional dressing
// here: Enforcer.Require on a nil receiver is FAIL-CLOSED (middleware.go:174-181 writes 404),
// so a chain built without WithAccess answers 404 to every publish and would make the
// assertions below pass without ever reaching the store.
func publishChain(d *testutil.DB) http.Handler {
	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
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
			WorkspaceID: pg.WorkspaceID, SpaceID: pg.SpaceID, SpaceCreatedBy: sp.CreatedBy,
			SpacePrivate: sp.Private, PageCreatedBy: pg.CreatedBy,
		}, nil
	}
	pageEnf := permission.NewEnforcer(permStore, permission.PageResolverFromParam("pageID", pageLooker, permStore))
	h := approval.NewHandler(approval.NewStore(d.Pool)).WithAccess(pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(apSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		h.Mount(r)
	})
	return r
}

func TestPublishApproved_LeavesTheRowByteIdentical_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Runbook")

	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`SELECT space_id FROM pages WHERE id = $1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("read space_id: %v", err)
	}

	// The oracle reads the row with SQL against the pool, never through a Store getter: an
	// oracle sharing a code path with its subject is not an independent oracle.
	row := func(label string) string {
		var j string
		if err := d.Pool.QueryRow(ctx,
			`SELECT to_jsonb(p)::text FROM pages p WHERE id = $1`, pageID).Scan(&j); err != nil {
			t.Fatalf("snapshot %s: %v", label, err)
		}
		return j
	}
	status := func() string {
		var s string
		if err := d.Pool.QueryRow(ctx,
			`SELECT doc_status FROM pages WHERE id = $1`, pageID).Scan(&s); err != nil {
			t.Fatalf("read doc_status: %v", err)
		}
		return s
	}

	chain := publishChain(d)
	publish := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost,
			"/v1/spaces/"+spaceID+"/pages/"+pageID+"/publish", nil)
		r.Header.Set("X-Gateway-Auth", apSecret)
		r.Header.Set("X-User-Email", "alice@corp.com")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, r)
		return rr
	}

	// PRECONDITION, ASSERTED: the seeded page really is a draft. Without this a publish that
	// silently did nothing on a missing page would read the same as the case under test.
	if got := status(); got != "draft" {
		t.Fatalf("seeded page doc_status = %q, want %q — fixture is wrong", got, "draft")
	}

	// POSITIVE CONTROL, AND IT IS THE ONE THAT PROVES THE PROBE ARRIVES. A 409 carrying the
	// store's own sentence can only come from PublishApproved's status guard, so the whole
	// chain — gatewayauth, authz, the page enforcer, the handler, the store — is live. A 404
	// here would mean the enforcer refused and the assertions below would be vacuous.
	draftRow := row("draft")
	rr := publish()
	if rr.Code != http.StatusConflict {
		t.Fatalf("publish on a draft = %d, want 409. body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if !strings.Contains(rr.Body.String(), "must be approved before publishing") {
		t.Fatalf("publish on a draft did not refuse for the expected reason: %s", strings.TrimSpace(rr.Body.String()))
	}
	if got := row("draft after refusal"); got != draftRow {
		t.Fatalf("a REFUSED publish still changed the row:\n before=%s\n after =%s", draftRow, got)
	}

	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET doc_status = 'approved' WHERE id = $1`, pageID); err != nil {
		t.Fatalf("seed approved: %v", err)
	}

	// THE MEASUREMENT.
	before := row("before publish")
	rr = publish()
	if rr.Code != http.StatusOK {
		t.Fatalf("publish on an approved page = %d, want 200. body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	after := row("after publish")

	if after != before {
		t.Fatalf("PUBLISH CHANGED THE PAGE ROW — the endpoint is no longer inert.\n"+
			"This test pins a MEASURED no-op, not a requirement: if publishing is meant to do\n"+
			"something now, that is a deliberate product change and this test should be replaced\n"+
			"with one asserting the new state (and W3.1 finding (11) revisited).\n"+
			" before=%s\n after =%s", before, after)
	}
	if got := status(); got != "approved" {
		t.Fatalf("doc_status after publish = %q, want %q", got, "approved")
	}

	// Idempotent for the same reason it is inert — stated separately so a future change that
	// makes the FIRST publish meaningful still has to answer for the second.
	rr = publish()
	if rr.Code != http.StatusOK {
		t.Fatalf("second publish = %d, want 200. body=%s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if got := row("after second publish"); got != before {
		t.Fatalf("the SECOND publish changed the row:\n before=%s\n after =%s", before, got)
	}
}
