package analytics_test

// THE SPACE ROLL-UP — THE THIRD OF THREE SCOPES THE PRODUCT PAGE SELLS, AND THE ONE THAT DID NOT
// EXIST.
//
// talyvor.higgsfield.app/products/docs lists, present tense, under "The detail":
//
//	+ PER-PAGE RUNNING AI COST
//	+ COST PER REVISION IN VERSION HISTORY
//	+ PAGE, SPACE AND ORG ROLLUPS               ← the SPACE third of this one
//
// MEASURED at merged main `9144e16`, before a line of this:
// `grep -rn 'spaces/{spaceID}/analytics|SpaceStats|SpaceRollup' internal/ frontend/src` was EMPTY.
// `GET /workspaces/{wsID}/analytics/pages` is the ORG roll-up and
// `GET /spaces/{spaceID}/pages/{pageID}/analytics` is the PAGE one; nothing in the repo grouped
// readership by space. W3.1 has been claimed four times and #190's own message recorded "rollups
// are real (internal/analytics)" — true of the org half, silent about the half this item names.
//
// ⚠ WHAT THE CENTRAL CASE IS. A space roll-up that quietly answers with the WHOLE WORKSPACE is
// the failure mode here, and it is not hypothetical: the store serves both scopes from ONE
// statement in which "every space" is expressed as an EMPTY spaceID. Any path that reaches that
// statement with "" — a renamed URL param, a handler reading the wrong key, a zero value — gets
// the org figures under a space's name and looks entirely normal doing it. `chi.URLParam` returns
// "" for a param that is not mounted, which is exactly how it would happen (internal/mountguard).
// So the fixture below seeds TWO spaces with DIFFERENT readership and the assertions are about
// what the space roll-up must NOT contain.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

// rollupChain mirrors main.go, including the two wirings the space roll-up depends on:
// `WithSpaceAccess` (the route's gate) and `WithPageRead` (the per-page visibility filter the org
// roll-up already needs). Building it here rather than reusing chainBoth keeps this file's
// subject explicit — if either wiring is dropped in production, `mainwiring_test.go` is what says
// so; this file says what the figures mean once it is wired.
func rollupChain(d *testutil.DB) http.Handler {
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
	spaceEnf := permission.NewEnforcer(permStore, permission.SpaceResolverFromParam("spaceID",
		func(ctx context.Context, id string) (permission.SpaceMeta, error) {
			sp, err := spaceStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
			if err != nil {
				return permission.SpaceMeta{}, err
			}
			return permission.SpaceMeta{WorkspaceID: sp.WorkspaceID, Private: sp.Private, CreatedBy: sp.CreatedBy}, nil
		}))

	aStore := analytics.NewStore(d.Pool)
	aStore.WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(pageLooker))
	ah := analytics.NewHandler(aStore).WithAccess(pageEnf).WithSpaceAccess(spaceEnf)

	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		exempt := func(p string) bool { return strings.HasPrefix(p, "/v1/public/") }
		r.Use(gatewayauth.Middleware(rollupSecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		ah.Mount(r)
	})
	return r
}

const rollupSecret = "sec4-test-gateway-secret-0123456789"

func rollupGet(path, email string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("X-Gateway-Auth", rollupSecret)
	r.Header.Set("X-User-Email", email)
	return r
}

// rollupBody decodes only what these cases are about. Explicit tags rather than a map: a renamed
// key would otherwise read as a zero, and every figure here is a claim about readership.
type rollupBody struct {
	TotalViews     int  `json:"total_views"`
	UniqueViewers  int  `json:"unique_viewers"`
	NeverRead      int  `json:"never_read_count"`
	MostReadPages  []pg `json:"most_read_pages"`
	LeastReadPages []pg `json:"least_read_pages"`
}

// ⚠ THE PER-ROW VIEW FIGURE IS `total_views`, NOT `view_count`, AND THE FIRST DRAFT OF THIS
// STRUCT SAID `view_count`. Nothing here failed: the field simply decoded to 0 on every row, and
// because the assertions below were about WHICH pages appear rather than how many views they had,
// the wrong key was invisible. `view_count` IS a real key in this API — on `ViewerStat` — which is
// what makes it read as correct. It was caught by `tsc --noEmit` on the SPA half, where the same
// mistake could not hide, and the figure is now ASSERTED so the name cannot rot back.
type pg struct {
	PageID     string `json:"page_id"`
	Title      string `json:"title"`
	TotalViews int    `json:"total_views"`
}

// view records one read of a page by one viewer, directly, so a case can build an exact readership
// shape without driving the record route for every hit.
func view(t *testing.T, d *testutil.DB, ws, pageID, viewer string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
        INSERT INTO page_views (page_id, workspace_id, viewer_id, viewer_name, duration_sec, created_at)
        VALUES ($1, $2, $3, $3, 10, NOW())`, pageID, ws, viewer,
	); err != nil {
		t.Fatalf("seed view of %s by %s: %v", pageID, viewer, err)
	}
}

// spaceOf returns a page's space id.
func spaceOf(t *testing.T, d *testutil.DB, pageID string) string {
	t.Helper()
	var s string
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&s); err != nil {
		t.Fatalf("read space of %s: %v", pageID, err)
	}
	return s
}

// movePageToNewSpace creates a second space in the same workspace and moves a page into it, so one
// workspace holds two spaces with distinct readership. `d.Page` always seeds into one space, and a
// fixture that cannot express two is a fixture in which a workspace-wide answer and a space-scoped
// one are the same bytes — the exact confusion these cases exist to detect.
func movePageToNewSpace(t *testing.T, d *testutil.DB, ws, owner, pageID, name string) string {
	t.Helper()
	var spaceID string
	if err := d.Pool.QueryRow(context.Background(), `
        INSERT INTO spaces (workspace_id, name, slug, created_by)
        VALUES ($1, $2, $3, $4) RETURNING id`,
		ws, name, strings.ToLower(name), owner,
	).Scan(&spaceID); err != nil {
		t.Fatalf("create space %q: %v", name, err)
	}
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE pages SET space_id = $2 WHERE id = $1`, pageID, spaceID); err != nil {
		t.Fatalf("move %s into %s: %v", pageID, spaceID, err)
	}
	return spaceID
}

// ⚠ THE CASE THIS FILE EXISTS FOR. Two spaces, different readership, and the space roll-up must
// report ONE of them. A roll-up that reached the shared statement with an empty scope would
// return the workspace's figures here and every number would look plausible.
func TestSpaceRollup_ReportsITSOwnSpaceAndNotTheWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	homePage := d.Page(t, ws, owner, "Home runbook")
	otherPage := d.Page(t, ws, owner, "Other space page")
	homeSpace := spaceOf(t, d, homePage)
	otherSpace := movePageToNewSpace(t, d, ws, owner, otherPage, "Operations")

	// Home space: 2 views by 2 distinct viewers. Other space: 5 views by 1. Every figure differs
	// between the two scopes, so no assertion below can be satisfied by the wrong one.
	view(t, d, ws, homePage, "u-a")
	view(t, d, ws, homePage, "u-b")
	for i := 0; i < 5; i++ {
		view(t, d, ws, otherPage, "u-c")
	}
	// ⚠ A PAGE NOBODY HAS OPENED, IN THE OTHER SPACE, AND IT IS HERE BECAUSE A CONTROL SAID SO.
	// The never-read cohort is a SECOND query with its own space predicate, and control C2
	// neutered that predicate while every assertion in this file stayed green: the original
	// fixture had no never-read pages at all, so the cohort was 0 whether it was scoped or not.
	// A cohort that is empty in both directions cannot tell you which one it measured.
	unreadElsewhere := d.Page(t, ws, owner, "Never opened, other space")
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE pages SET space_id = $2 WHERE id = $1`, unreadElsewhere, otherSpace); err != nil {
		t.Fatalf("move the unread page into the other space: %v", err)
	}

	chain := rollupChain(d)

	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, rollupGet("/v1/spaces/"+homeSpace+"/analytics/pages", "owner@corp.com"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET space analytics: HTTP %d — %s\n\nThe product page sells PAGE, SPACE AND ORG "+
			"ROLLUPS; the org and page scopes exist and nothing groups readership by space.",
			rr.Code, rr.Body.String())
	}
	var got rollupBody
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode space roll-up: %v — %s", err, rr.Body.String())
	}

	if got.TotalViews != 2 {
		t.Errorf("total_views = %d, want 2 — the home space has two views and the workspace has "+
			"seven; %d is the WORKSPACE answer under a space's name", got.TotalViews, got.TotalViews)
	}
	if got.UniqueViewers != 2 {
		t.Errorf("unique_viewers = %d, want 2 (u-a and u-b read the home space; u-c read only the "+
			"other one)", got.UniqueViewers)
	}
	for _, row := range append(append([]pg{}, got.MostReadPages...), got.LeastReadPages...) {
		if row.PageID == otherPage {
			t.Errorf("the space roll-up lists %q (%s), which lives in a DIFFERENT space — the "+
				"scope reached the query empty and this is the workspace ranking", row.Title, row.PageID)
		}
	}
	if len(got.MostReadPages) != 1 || got.MostReadPages[0].PageID != homePage {
		t.Errorf("most_read_pages = %+v, want exactly the home space's one page (%s)",
			got.MostReadPages, homePage)
	} else if got.MostReadPages[0].TotalViews != 2 {
		// The ranked row's own figure, asserted rather than merely decoded — this is the field
		// whose name was wrong and silently zero until the SPA's typecheck found it. 2 is the home
		// space's readership; 7 would be the workspace's, and 5 the other space's.
		t.Errorf("the ranked row reports total_views = %d, want 2 — the row is the right page with "+
			"someone else's number", got.MostReadPages[0].TotalViews)
	}

	if got.NeverRead != 0 {
		t.Errorf("never_read_count = %d, want 0 — the home space's one page HAS been read; the "+
			"unread page lives in the other space, so a non-zero count here is the WORKSPACE's "+
			"cohort under this space's name", got.NeverRead)
	}

	// ⚠ AND THE ORG ROLL-UP MUST STILL SEE BOTH — otherwise "the space roll-up is correct" would
	// be satisfied by a change that narrowed EVERY scope, which is the same defect pointing the
	// other way and would be invisible to every assertion above.
	orr := httptest.NewRecorder()
	chain.ServeHTTP(orr, rollupGet("/v1/workspaces/"+ws+"/analytics/pages", "owner@corp.com"))
	if orr.Code != http.StatusOK {
		t.Fatalf("GET workspace analytics: HTTP %d — %s", orr.Code, orr.Body.String())
	}
	var org rollupBody
	if err := json.Unmarshal(orr.Body.Bytes(), &org); err != nil {
		t.Fatalf("decode org roll-up: %v", err)
	}
	if org.TotalViews != 7 {
		t.Errorf("the ORG roll-up now reports total_views = %d, want 7 — the space narrowing "+
			"leaked into the workspace scope", org.TotalViews)
	}
	if len(org.MostReadPages) != 2 {
		t.Errorf("the ORG roll-up ranks %d pages, want 2 — it should still see both spaces",
			len(org.MostReadPages))
	}
	if org.NeverRead != 1 {
		t.Errorf("the ORG roll-up reports never_read_count = %d, want 1 — the unread page is in "+
			"the workspace even though it is not in the home space, and this is the assertion "+
			"that makes the space-scoped 0 above mean something rather than being a cohort that "+
			"is empty everywhere", org.NeverRead)
	}
}

// ⚠ AGREEMENT BY CONSTRUCTION, ASSERTED RATHER THAN ASSUMED. The two roll-ups are one statement
// narrowed, and the whole argument for that (rather than a second implementation) is that they
// cannot drift. In a workspace with exactly ONE space the two scopes describe the same set, so
// every figure must match — and this compares them to EACH OTHER rather than to a constant, so it
// keeps holding as the roll-up's contents change.
func TestSpaceRollup_AgreesWithTheOrgRollupWhenTheWorkspaceHasOneSpace_RealPG(t *testing.T) {
	d := testutil.New(t)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	p1 := d.Page(t, ws, owner, "Alpha")
	p2 := d.Page(t, ws, owner, "Beta")
	only := spaceOf(t, d, p1)
	// ⚠ `d.Page` MAKES A NEW SPACE EACH TIME, which the premise assertion below caught rather than
	// my reading it — the first draft of this case assumed two pages in one workspace shared a
	// space, and "agreement between the two scopes" would then have been a comparison between a
	// one-page space and a two-page workspace that could never match. The move makes the two
	// scopes describe the same SET, which is the only condition under which agreement means
	// anything.
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE pages SET space_id = $2 WHERE id = $1`, p2, only); err != nil {
		t.Fatalf("put both pages in one space: %v", err)
	}
	if s2 := spaceOf(t, d, p2); s2 != only {
		t.Fatalf("premise: the two pages are still in different spaces (%s, %s), so the two scopes "+
			"are not the same set and agreement would prove nothing", only, s2)
	}
	view(t, d, ws, p1, "u-a")
	view(t, d, ws, p1, "u-b")
	view(t, d, ws, p2, "u-a")

	chain := rollupChain(d)
	fetch := func(path string) rollupBody {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, rollupGet(path, "owner@corp.com"))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: HTTP %d — %s", path, rr.Code, rr.Body.String())
		}
		var b rollupBody
		if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return b
	}

	spaceR := fetch("/v1/spaces/" + only + "/analytics/pages")
	orgR := fetch("/v1/workspaces/" + ws + "/analytics/pages")

	// The premise: there is readership to compare. Two empty roll-ups agree perfectly.
	if orgR.TotalViews == 0 {
		t.Fatalf("premise: the org roll-up reports no views, so agreement below is agreement "+
			"between two empty answers: %+v", orgR)
	}
	if spaceR.TotalViews != orgR.TotalViews {
		t.Errorf("total_views: space %d, org %d — one workspace, one space, two answers",
			spaceR.TotalViews, orgR.TotalViews)
	}
	if spaceR.UniqueViewers != orgR.UniqueViewers {
		t.Errorf("unique_viewers: space %d, org %d", spaceR.UniqueViewers, orgR.UniqueViewers)
	}
	if spaceR.NeverRead != orgR.NeverRead {
		t.Errorf("never_read_count: space %d, org %d", spaceR.NeverRead, orgR.NeverRead)
	}
	if len(spaceR.MostReadPages) != len(orgR.MostReadPages) {
		t.Errorf("most_read_pages: space ranks %d, org ranks %d",
			len(spaceR.MostReadPages), len(orgR.MostReadPages))
	}
}

// ⚠ THE INERT-SCOPE GUARD, AT THE STORE RATHER THAN AT THE ROUTE. `getScopedStats` expresses "every
// space" as an empty id, which is what lets one statement serve both roll-ups; that convenience is
// also a trapdoor, because a space roll-up reached with "" answers with the whole workspace and
// looks normal. `GetSpaceStats` refuses instead of widening, and this is the assertion that keeps
// it refusing.
func TestGetSpaceStats_RefusesAnEmptyScopeInsteadOfWideningToTheWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	p := d.Page(t, ws, owner, "Alpha")
	view(t, d, ws, p, "u-a")

	permStore := permission.NewStore(d.Pool)
	spaceStore := space.NewStore(d.Pool)
	pageStore := page.NewStore(d.Pool)
	st := analytics.NewStore(d.Pool)
	st.WithPageRead(spaceauth.New(spaceStore, permStore).WithPageMeta(
		func(ctx context.Context, id string) (permission.PageMeta, error) {
			pgm, err := pageStore.GetByIDInWorkspaces(ctx, id, authz.WorkspaceIDs(ctx))
			if err != nil {
				return permission.PageMeta{}, err
			}
			return permission.PageMeta{WorkspaceID: pgm.WorkspaceID, SpaceID: pgm.SpaceID}, nil
		}))

	got, err := st.GetSpaceStats(context.Background(), ws, "", 30)
	if err == nil {
		t.Fatalf("GetSpaceStats with an EMPTY space id returned figures instead of refusing: "+
			"%+v — an empty scope means \"every space\" in the shared statement, so this is the "+
			"whole workspace reported as one space's roll-up", got)
	}
	if !strings.Contains(err.Error(), "space id") {
		t.Errorf("err = %v, want one that names the missing space scope", err)
	}
}

// ⚠ A SPACE IN ANOTHER WORKSPACE IS NOT READABLE, and the premise is asserted first: without it,
// a refusal is equally consistent with a route that is broken for everybody.
func TestSpaceRollup_RefusesASpaceInAnotherWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)

	victimWS := d.Workspace(t)
	victim := d.Member(t, victimWS, "victim@corp.com")
	victimPage := d.Page(t, victimWS, victim, "Acquisition")
	victimSpace := spaceOf(t, d, victimPage)
	view(t, d, victimWS, victimPage, "u-a")

	attackerWS := d.Workspace(t)
	d.Member(t, attackerWS, "attacker@corp.com")

	chain := rollupChain(d)

	orr := httptest.NewRecorder()
	chain.ServeHTTP(orr, rollupGet("/v1/spaces/"+victimSpace+"/analytics/pages", "victim@corp.com"))
	if orr.Code != http.StatusOK {
		t.Fatalf("premise: the space's OWN member cannot read its roll-up either (HTTP %d) — the "+
			"refusal below would be evidence of nothing: %s", orr.Code, orr.Body.String())
	}
	var owned rollupBody
	if err := json.Unmarshal(orr.Body.Bytes(), &owned); err != nil {
		t.Fatalf("decode owner roll-up: %v", err)
	}
	if owned.TotalViews == 0 {
		t.Fatalf("premise: the owner's roll-up reports no readership, so a leak would have "+
			"nothing to leak: %s", orr.Body.String())
	}

	arr := httptest.NewRecorder()
	chain.ServeHTTP(arr, rollupGet("/v1/spaces/"+victimSpace+"/analytics/pages", "attacker@corp.com"))
	if arr.Code == http.StatusOK {
		t.Fatalf("a member of another workspace read this space's readership roll-up: HTTP 200 — %s",
			arr.Body.String())
	}
}

// ⚠ A PRIVATE SPACE THE CALLER HAS NO GRANT ON, INSIDE THEIR OWN WORKSPACE — AND THIS CASE EXISTS
// BECAUSE A CONTROL PROVED THE OTHER ONE COULD NOT SEE IT.
//
// `SpaceStats` is protected twice: the route's SPACE enforcer (may this caller VIEW this space —
// a question about grants) and the handler's workspace assertion (is that space in a workspace
// this session is a verified member of — a question about tenancy). Control C4 REMOVED the space
// enforcer from the route and `TestSpaceRollup_RefusesASpaceInAnotherWorkspace_RealPG` stayed
// GREEN, correctly: an attacker in a different workspace is stopped by the tenancy half, so that
// test proves one gate exists rather than two.
//
// The gate the space enforcer alone holds is this one — a colleague in the SAME workspace, whom
// tenancy admits, reading a private space they were never granted. It is the exact shape
// `privatespace_realpg_test.go` records for the org roll-up: before that gate existed, a
// workspace member with no grant on a private space received its page titles, ids and view
// counts, while the by-page route 403'd the same caller for the same page.
func TestSpaceRollup_RefusesAPrivateSpaceInTheCallersOwnWorkspace_RealPG(t *testing.T) {
	d := testutil.New(t)

	ws := d.Workspace(t)
	owner := d.Member(t, ws, "owner@corp.com")
	outsider := d.Member(t, ws, "outsider@corp.com")
	_ = outsider

	secretPage := d.Page(t, ws, owner, "Acquisition memo")
	secretSpace := spaceOf(t, d, secretPage)
	view(t, d, ws, secretPage, "u-a")

	chain := chainForPrivacy(t, d, secretSpace)

	// ⚠ THE PREMISE, ASSERTED BEFORE THE REFUSAL. The space's own creator must be able to read the
	// roll-up while it is private; without this the refusal below is equally consistent with a
	// route that is broken for everyone, or with a space holding no readership to leak.
	orr := httptest.NewRecorder()
	chain.ServeHTTP(orr, rollupGet("/v1/spaces/"+secretSpace+"/analytics/pages", "owner@corp.com"))
	if orr.Code != http.StatusOK {
		t.Fatalf("premise: the private space's OWN creator cannot read its roll-up (HTTP %d) — the "+
			"refusal below would be evidence of nothing: %s", orr.Code, orr.Body.String())
	}
	var owned rollupBody
	if err := json.Unmarshal(orr.Body.Bytes(), &owned); err != nil {
		t.Fatalf("decode creator roll-up: %v", err)
	}
	if owned.TotalViews == 0 {
		t.Fatalf("premise: the creator's roll-up reports no readership, so there is nothing for a "+
			"leak to disclose: %s", orr.Body.String())
	}

	// The colleague: a verified member of the SAME workspace, with no grant on this space. Tenancy
	// admits them; only the space gate does not.
	arr := httptest.NewRecorder()
	chain.ServeHTTP(arr, rollupGet("/v1/spaces/"+secretSpace+"/analytics/pages", "outsider@corp.com"))
	if arr.Code == http.StatusOK {
		t.Fatalf("a workspace member with NO grant on a private space read its readership roll-up: "+
			"HTTP 200 — %s\n\nThe tenancy assertion admits them (they are in the workspace); the "+
			"space enforcer is the only thing that does not.", arr.Body.String())
	}
}

// chainForPrivacy builds the production chain and marks one space private, so the space enforcer
// has something to refuse. Private is set directly because the roll-up is a READ path and this
// file's subject is what it discloses, not how a space becomes private.
func chainForPrivacy(t *testing.T, d *testutil.DB, spaceID string) http.Handler {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE spaces SET private = true WHERE id = $1`, spaceID); err != nil {
		t.Fatalf("mark space private: %v", err)
	}
	var private bool
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT private FROM spaces WHERE id = $1`, spaceID).Scan(&private); err != nil || !private {
		t.Fatalf("premise: the space is not private (private=%v err=%v), so the enforcer has "+
			"nothing to refuse and this file's central case would pass on an open space",
			private, err)
	}
	return rollupChain(d)
}
