package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/pagelock"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// A SERVER-OWNED MONEY COLUMN WAS IN THE PATCH ALLOWLIST — the same class as this
// package's is_admin bug (sec_isadmin_update_test.go), one key along.
//
// page.Handler.Update does `json.NewDecoder(r.Body).Decode(&updates)`: the map IS the
// request body. page.Store.Update then writes any key present in `updatableFields`, and
// that map listed `ai_cost_usd`.
//
// pages.ai_cost_usd is not an editable property of a document. It is a REPORT: the
// trackintegration syncer sums the AI cost of the Track issues linked to the page and
// writes the total through Store.UpdateAICost, a dedicated raw-SQL writer. model.Page
// derives `total_ai_cost_usd = ai_cost_usd + own_ai_cost_usd` on read, and that derived
// total is what PageView, SearchModal and the MCP page projection put in front of a human
// or an agent.
//
// MEASURED at 6f4aabb on real Postgres, through the production chain (gatewayauth + authz
// + permission.Enforcer), before the fix:
//
//	PATCH {"ai_cost_usd":999.99}  -> HTTP 200, column = 999.99,
//	                                 response `"ai_cost_usd":999.99,"total_ai_cost_usd":999.99`
//	PATCH {"ai_cost_usd":-1000}   -> HTTP 200, column = -1000; with a real own_ai_cost_usd
//	                                 of 42.00 underneath, total_ai_cost_usd read -958
//
// and the caller was BOB — a plain Edit-tier member, not an admin and not the page's
// author. The negative direction is the one that matters: it does not merely inflate a
// number, it CANCELS the page's own recorded Lens spend, which is real money observed from
// the Lens ledger by a completely different sweep (internal/lensintegration/pagecost.go).
//
// ⚠ WHAT THIS IS NOT, STATED SO NOBODY UPGRADES IT: no gate anywhere in this repository
// reads ai_cost_usd. internal/ratelimit's own header says so in as many words — "there is
// no balance check, no quota and no cost cap anywhere in this repository; pages.ai_cost_usd
// is a report synced back from Track, never read to decide". So this is reporting integrity,
// NOT privilege escalation, and it is confined to pages the caller may already edit — the
// cross-workspace door is shut elsewhere (sec4_crosstenant_test.go) and is not re-probed here.
//
// ⚠ AND IT IS NOT SELF-HEALING IN THE DEPLOYMENT THE README DOCUMENTS. The syncer rewrites
// every page each tick, so a forged value survives ~15 minutes where Track cost sync is
// configured — but Syncer.Start returns immediately when it is not, and Docs is documented
// to run standalone. There the forged number is permanent and nothing anywhere disagrees
// with it.
//
// THE FIX IS THE ALLOWLIST ENTRY, NOT A NEW REFUSAL. An un-allowlisted key is already
// ignored in silence by this store, which is exactly what the SIBLING money column
// own_ai_cost_usd has always done — measured in the same run, `PATCH
// {"own_ai_cost_usd":777.77}` returned 200 and moved nothing. Bringing ai_cost_usd into
// line with its sibling is a smaller change than inventing a rejection path, and
// [EDIT-STILL-WORKS] below pins that the rest of a mixed request still lands.
func TestSec_Update_AICostIsNotWritableFromTheRequestBody(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com") // space creator ⇒ Admin on its pages
	pageID := d.Page(t, ws, alice, "Original title")

	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatalf("lookup space: %v", err)
	}

	permStore := permission.NewStore(d.Pool)

	// Bob is an ORDINARY Edit-tier member: not an admin, not the author. The defect must be
	// shown at the tier that has it, not at the one that could plausibly be trusted with it.
	bobEmail := "bob@corp.com"
	d.Member(t, ws, bobEmail)
	var bobID string
	if err := d.Pool.QueryRow(ctx,
		`SELECT member_id FROM workspace_members WHERE workspace_id=$1 AND email=$2`,
		ws, bobEmail).Scan(&bobID); err != nil {
		t.Fatalf("lookup bob: %v", err)
	}
	if _, err := permStore.Grant(ctx, permission.Permission{
		ResourceType: permission.ResourceSpace, ResourceID: spaceID,
		SubjectType: "member", SubjectID: bobID,
		Access: permission.AccessEdit, WorkspaceID: ws, GrantedBy: alice,
	}); err != nil {
		t.Fatalf("grant edit to bob: %v", err)
	}

	// Chain mirroring main.go, same wiring as sec_isadmin_update_test.go.
	lockStore := pagelock.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool).WithGuard(lockStore)
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
	pageHandler := page.NewHandler(pageStore, d.Pool)
	pageHandler.WithAccess(pageEnf, pageEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		pageHandler.Mount(r)
	})

	do := func(method, path, email, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gateway-Auth", testGatewaySecret)
		req.Header.Set("X-User-Email", email)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	base := "/v1/spaces/" + spaceID + "/pages/" + pageID

	costs := func() (track, own float64) {
		t.Helper()
		if err := d.Pool.QueryRow(ctx,
			`SELECT ai_cost_usd, own_ai_cost_usd FROM pages WHERE id=$1`, pageID).Scan(&track, &own); err != nil {
			t.Fatalf("read costs: %v", err)
		}
		return
	}
	titleNow := func() string {
		t.Helper()
		var s string
		if err := d.Pool.QueryRow(ctx, `SELECT title FROM pages WHERE id=$1`, pageID).Scan(&s); err != nil {
			t.Fatalf("read title: %v", err)
		}
		return s
	}

	// A REAL own-AI cost underneath, so the negative case has something to cancel. This is
	// seeded through SQL rather than through PriceAISpend on purpose: what is under test is
	// the PATCH door, and a fixture that had to drive the Lens pricing sweep to get here
	// would fail for reasons that have nothing to do with it.
	const realOwnCost = 42.00
	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET own_ai_cost_usd = $1 WHERE id = $2`, realOwnCost, pageID); err != nil {
		t.Fatalf("seed own cost: %v", err)
	}
	trackBefore, ownBefore := costs()
	if ownBefore != realOwnCost {
		t.Fatalf("premise: own_ai_cost_usd = %v, want %v — the fixture did not take", ownBefore, realOwnCost)
	}

	// ── THE DEFECT ────────────────────────────────────────────────────────────────────
	// One mixed request: the forged money field AND an ordinary editable field, because
	// the fix must drop exactly one key rather than refuse the whole PATCH.
	rr := do(http.MethodPatch, base, bobEmail, `{"ai_cost_usd":-1000,"title":"bob's honest edit"}`)

	// [HONEST-200] — the floor. "The column did not move" is satisfied by a 403, a 404 and
	// a route that was never mounted; without this, every assertion below could be earned
	// by the request never reaching the store at all.
	if rr.Code != http.StatusOK {
		t.Fatalf("[HONEST-200] PATCH by an Edit-tier member = %d, want 200 — the assertions "+
			"below cannot distinguish a working gate from a refused request. body=%s",
			rr.Code, rr.Body.String())
	}

	// [NOT-WRITABLE] — the money column, and the derived total a human actually reads.
	trackAfter, ownAfter := costs()
	if trackAfter != trackBefore {
		t.Errorf("[NOT-WRITABLE] ai_cost_usd moved on a request body: %v -> %v. It is a report "+
			"written by trackintegration.Syncer through Store.UpdateAICost, not a property of "+
			"the document, and no member may set it.", trackBefore, trackAfter)
	}
	if got := trackAfter + ownAfter; got != realOwnCost {
		t.Errorf("[NOT-WRITABLE] total_ai_cost_usd is %v, want %v — a member cancelled the "+
			"page's own recorded Lens spend by sending a negative Track half.", got, realOwnCost)
	}
	var body struct {
		AICostUSD      float64 `json:"ai_cost_usd"`
		OwnAICostUSD   float64 `json:"own_ai_cost_usd"`
		TotalAICostUSD float64 `json:"total_ai_cost_usd"`
		Title          string  `json:"title"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("[NOT-WRITABLE] decode PATCH response: %v — body=%s", err, rr.Body.String())
	}
	// The response is built from the UPDATE's RETURNING row, so today this cannot fail while
	// the two DB checks above pass. It is here because the WIRE is what PageView, SearchModal
	// and the MCP page projection render, and it separates from them the day the response
	// stops being derived from the row. Scored as a witness, not as an independent guard.
	if body.TotalAICostUSD != realOwnCost {
		t.Errorf("[NOT-WRITABLE] the PATCH response reported total_ai_cost_usd = %v, want %v",
			body.TotalAICostUSD, realOwnCost)
	}

	// [EDIT-STILL-WORKS] — the co-submitted ordinary field landed. A fix that rejected the
	// whole request, or dropped every key alongside the forged one, would satisfy
	// [NOT-WRITABLE] and break editing.
	if got := titleNow(); got != "bob's honest edit" {
		t.Errorf("[EDIT-STILL-WORKS] title = %q, want %q — ignoring ai_cost_usd must not "+
			"discard the rest of the request.", got, "bob's honest edit")
	}

	// [SIBLING-PARITY] — own_ai_cost_usd, the other money column, has never been in the
	// allowlist and must stay out of it. This is the behaviour ai_cost_usd is being brought
	// into line with, so it is pinned rather than assumed.
	if rr := do(http.MethodPatch, base, bobEmail, `{"own_ai_cost_usd":777.77}`); rr.Code != http.StatusOK {
		t.Fatalf("[SIBLING-PARITY] premise: PATCH own_ai_cost_usd = %d, want 200", rr.Code)
	}
	if _, own := costs(); own != realOwnCost {
		t.Errorf("[SIBLING-PARITY] own_ai_cost_usd moved on a request body: %v -> %v. It is "+
			"incremented per Lens ledger row by internal/lensintegration and is not editable.",
			realOwnCost, own)
	}

	// ⚠ THERE IS DELIBERATELY NO "the syncer can still write the column" ASSERTION HERE, AND
	// IT WAS WRITTEN AND THEN DELETED RATHER THAN NEVER CONSIDERED. Closing the client door
	// must not close the server one, so this test originally ended by driving
	// Store.UpdateAICost and reading the value back. Control C6 (UpdateAICost made a no-op)
	// measured what actually holds that invariant: SEVEN tests go red, six of them in
	// internal/trackintegration — TestSyncPageCosts_APageWithNoLinksIsWrittenAsZero,
	// _ATrackFailureLeavesThePreviousTotalIntact, _CoversEveryWorkspace_NotJustThePinnedOne,
	// _WithoutMemberSyncEnumeratesFromContentWithoutCallingTrack,
	// TestSweep_LinkedIssueOverwriteNeverErasesAccumulatedOwnCost and
	// TestOwnAICost_LinkedIssueSweepDoesNotEraseTheOwnCost. An assertion that no control can
	// claim alone is a second copy of somebody else's guard, and this one had already done
	// harm: it read own_ai_cost_usd as the constant seeded above rather than as the value on
	// the row, so control C3 — which makes own_ai_cost_usd writable — reddened it for a
	// reason that had nothing to do with the syncer. C6 is kept in the harness as a
	// must-stay-green BLIND control instead: it must be caught, and NOT by this file.
}
