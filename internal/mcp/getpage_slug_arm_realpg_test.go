package mcp_test

// THE SECOND ARM OF get_page, WHICH NOTHING HAS EVER DRIVEN.
//
// `get_page` takes EITHER `page_id` OR `slug` + `space_id`. Those are not two spellings of one
// path — they are two paths, and they diverge at every layer that matters:
//
//	                        page_id arm                     slug arm
//	  lookup                pages.GetByID(id)               pages.GetBySlug(space_id, slug)
//	  workspace resolution  Server.pageWorkspace(page_id)   Server.spaceWorkspace(space_id)
//	  required args         page_id                         slug AND space_id
//
// The authz chokepoint resolves a tool's workspace BEFORE dispatch, and for this tool it resolves
// it from a DIFFERENT OBJECT depending on which arm the caller took: the page for one, the space
// for the other. Only the in-tool `canViewPage` on the RESOLVED row is common to both.
//
// ⚠ MEASURED, NOT ASSUMED: of the 14 `get_page` calls across every test in internal/mcp, ZERO
// supply a slug. Every tenancy case this tool has — cross-workspace arg-trust, fail-closed on a
// nonexistent object, private-space-without-grant — was written against the page_id arm alone. The
// slug arm's lookup, its workspace resolution and its argument branch are exercised by nothing.
//
// ⚠⚠ THIS TEST WAS GREEN THE FIRST TIME IT RAN AND THAT IS REPORTED, NOT HIDDEN. It closes a
// COVERAGE gap, not a live defect: the slug arm is correctly gated today. What makes it worth
// shipping is that nothing said so — and that the four findings this item has already merged
// (#82/#83/#84/#85) were all code that looked right and was held by no test. The controls in
// scripts/w31-mcp-slugarm-controls.py are what earn it: each mutates a line ONLY the slug arm
// reaches, and the id-arm tests stay green under every one of them.
//
// WHY AN MCP GUARD CANNOT BE A routeguard GUARD. internal/routeguard polices HTTP routes gated by
// an enforcer whose param is in the PATH. The MCP surface has no path at all — every tool takes
// its object from JSON-RPC arguments — so routeguard cannot describe a single one of these ten
// tools, and no future extension of it will. This surface's invariants have to be driven.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// seedPageWithSlug is this file's own seeder because the slug is the ARGUMENT UNDER TEST here, and
// the shared seedPageP derives it ("pg-<title>-<ws>") rather than letting the caller name it. A
// test about a lookup key must control that key.
func seedPageWithSlug(t *testing.T, d *testutil.DB, wsID, spaceID, author, title, slug, body string) string {
	t.Helper()
	var id string
	if err := d.Pool.QueryRow(context.Background(),
		`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text, content)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		spaceID, wsID, title, slug, author, body, `{"type":"doc"}`).Scan(&id); err != nil {
		t.Fatalf("seed page %q (slug %q): %v", title, slug, err)
	}
	return id
}

func TestMCP_GetPageSlugArm_IsGatedLikeTheIDArm_RealPG(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	bob := d.Member(t, wsB, "bob@corp.com")

	// ⚠ THE SAME SLUG IN BOTH WORKSPACES. A slug is unique per SPACE, not globally, so "the caller
	// named a slug they are entitled to" and "the caller named a slug someone else owns" can be
	// the SAME STRING. That is the property the page_id arm cannot express at all — an id is
	// globally unique, so a cross-tenant id is self-evidently foreign, while a cross-tenant slug
	// is indistinguishable from an honest one until the space is resolved.
	const shared = "quarterly-plan"

	spaceA := seedSpaceP(t, d, wsA, alice, "A Public", false)
	_ = seedPageWithSlug(t, d, wsA, spaceA, alice, "A Plan", shared, "ALICE-OWN-CONTENT")

	spaceB := seedSpaceP(t, d, wsB, bob, "B Public", false)
	_ = seedPageWithSlug(t, d, wsB, spaceB, bob, "B Plan", shared, "TOPSECRET-B-CONTENT")

	// A PRIVATE space in ALICE'S OWN workspace that she has no grant on — the within-workspace
	// ring, which a cross-workspace case cannot answer.
	carol := d.Member(t, wsA, "carol@corp.com")
	privSpace := seedSpaceP(t, d, wsA, carol, "A Board Private", true)
	_ = seedPageWithSlug(t, d, wsA, privSpace, carol, "Severance", "severance-model", "PRIVATE-BOARD-CONTENT")

	chain := newMCPChain(t, d)

	leaksB := func(rr *httptest.ResponseRecorder) bool {
		return strings.Contains(rr.Body.String(), "TOPSECRET-B-CONTENT")
	}
	leaksPrivate := func(rr *httptest.ResponseRecorder) bool {
		return strings.Contains(rr.Body.String(), "PRIVATE-BOARD-CONTENT")
	}
	denied := func(rr *httptest.ResponseRecorder) bool {
		b := rr.Body.String()
		return strings.Contains(b, "not a member") || strings.Contains(b, "not authorized") ||
			strings.Contains(b, "not found")
	}

	// ── [S-HONEST] THE PREMISE. Alice reaches her OWN page by slug. Asserted FIRST: every refusal
	// below is worthless if the slug arm simply does not work, and a fix that broke it outright
	// would pass every leak assertion in this file.
	rr := callTool(chain, "alice@corp.com", true, "get_page",
		map[string]any{"slug": shared, "space_id": spaceA})
	if !strings.Contains(rr.Body.String(), "ALICE-OWN-CONTENT") {
		t.Fatalf("[S-HONEST] alice could not read her OWN page by slug — the arm under test does "+
			"not work, so nothing else here means anything:\n%s", rr.Body.String())
	}
	if leaksB(rr) {
		t.Errorf("[S-HONEST] alice's own-slug read returned WORKSPACE B's content — GetBySlug is " +
			"not scoped by the space it was given")
	}

	// ── [S-XWS-SLUG] Alice names workspace B's SPACE and the shared slug. The chokepoint resolves
	// this tool's workspace from space_id on this arm, so this is the assertion that
	// spaceWorkspace + membership is what stands between her and B's document.
	rr = callTool(chain, "alice@corp.com", true, "get_page",
		map[string]any{"slug": shared, "space_id": spaceB})
	if leaksB(rr) || !denied(rr) {
		t.Errorf("[S-XWS-SLUG] alice (member of A only) reached workspace B's page through the "+
			"SLUG arm: leaked=%v denied=%v\n%s", leaksB(rr), denied(rr), rr.Body.String())
	}

	// ── [S-XWS-NOSPACE] The same slug with NO space_id. The arm requires space_id; the failure
	// must be a refusal, never a global slug lookup that finds B's row.
	rr = callTool(chain, "alice@corp.com", true, "get_page", map[string]any{"slug": shared})
	if leaksB(rr) {
		t.Errorf("[S-XWS-NOSPACE] a slug with no space_id returned another workspace's content — "+
			"the lookup fell back to a global slug search:\n%s", rr.Body.String())
	}

	// ── [S-PRIV-SLUG] Same workspace, PRIVATE space, no grant. The chokepoint passes here (alice
	// IS a member of wsA), so this case is held by the in-tool canViewPage on the resolved row and
	// by nothing else — a different guard from the one [S-XWS-SLUG] exercises.
	rr = callTool(chain, "alice@corp.com", true, "get_page",
		map[string]any{"slug": "severance-model", "space_id": privSpace})
	if leaksPrivate(rr) || !denied(rr) {
		t.Errorf("[S-PRIV-SLUG] alice has NO grant on carol's private space and reached its page "+
			"through the SLUG arm: leaked=%v denied=%v\n%s",
			leaksPrivate(rr), denied(rr), rr.Body.String())
	}

	// ── [S-NOAUTH-SLUG] No transit proof → 401 before dispatch, on this arm too.
	rr = callTool(chain, "alice@corp.com", false, "get_page",
		map[string]any{"slug": shared, "space_id": spaceB})
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("[S-NOAUTH-SLUG] no-auth slug-arm get_page = %d, want 401; leaked=%v",
			rr.Code, leaksB(rr))
	}

	// ── [S-GRANT] NOT OVER-CORRECTED. An explicit view grant on the private space makes the same
	// slug call succeed. Granted LAST so every case above ran while alice was still denied; this
	// is the assertion a blanket "refuse the slug arm" fix fails.
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO permissions (resource_type, resource_id, subject_type, subject_id, access, workspace_id)
		 VALUES ('space', $1, 'member', $2, 'view', $3)`, privSpace, alice, wsA); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rr = callTool(chain, "alice@corp.com", true, "get_page",
		map[string]any{"slug": "severance-model", "space_id": privSpace})
	if !leaksPrivate(rr) {
		t.Errorf("[S-GRANT] OVER-CORRECTION: alice holds an explicit 'view' grant on the private "+
			"space and the SLUG arm still will not return it:\n%s", rr.Body.String())
	}
}

// TestMCP_GetPageSlugArm_ArgumentBranchIsReachable guards the FIXTURE, not the product.
//
// Every refusal above would also pass if the slug arm were never entered — if `slug` were the
// wrong argument name, or if supplying both arms silently took the page_id path, every "denied"
// assertion would be satisfied by a request that measured nothing. This asserts the arm is really
// the one being exercised, by reaching a page whose id the caller never names and which is
// findable ONLY by its slug.
func TestMCP_GetPageSlugArm_ArgumentBranchIsReachable(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	space := seedSpaceP(t, d, ws, alice, "Reachable", false)
	seedPageWithSlug(t, d, ws, space, alice, "Only By Slug", "only-by-slug", "SLUGARM-REACHED")

	chain := newMCPChain(t, d)
	rr := callTool(chain, "alice@corp.com", true, "get_page",
		map[string]any{"slug": "only-by-slug", "space_id": space})
	if !strings.Contains(rr.Body.String(), "SLUGARM-REACHED") {
		t.Fatalf("[S-REACHABLE] the slug arm did not return a page reachable only by slug — every "+
			"refusal assertion in this file could be passing on a request that never entered it:\n%s",
			rr.Body.String())
	}

	// ⚠ AN ASSERTION WAS DELETED HERE RATHER THAN KEPT AS DECORATION. It checked that a slug with
	// no space_id does not resolve a page. The control that removes the `space_id required`
	// refusal left it GREEN — with an empty space_id the lookup simply matches no row, so the
	// assertion passes whether that branch exists or not. Nothing can make it fail, so it said
	// nothing. The bare-slug case that IS load-bearing is [S-XWS-NOSPACE] above, which asserts the
	// stronger property: it must not return ANOTHER WORKSPACE's content.
}
