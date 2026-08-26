package space_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
)

// The space + page ROUTE TABLE, stated as a contract.
//
// ⚠ WHY IT IS WORTH PINNING. These prefixes are NOT uniform, and something outside this repo
// depends on knowing that. The suite's BFF proxies /api/docs/* here, and when Docs went
// per-identity it applied a workspace prefix to every Docs call — reading the rule off the one
// route that has it. Five routes then addressed paths this router does not register, so opening
// a space returned Go's default `404 page not found`, and the suite reported "Docs is not
// configured on this deployment" while Docs was running and had just served the space list.
//
// This repo cannot test the BFF, and the BFF cannot see this router. What it CAN do is state
// the table exactly, so a change to it is a visible diff in a file whose comment says who
// breaks. THE ASYMMETRY IS THE POINT: only the LIST is workspace-scoped.
//
// It walks the mounted router rather than transcribing the Mount calls — a transcription can
// drift from what is registered, which is the class of bug this file is about.
func TestSpaceAndPageRouteTable(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		// Handlers with nil stores: nothing is served, and nothing needs to be. The subject is
		// which PATTERNS exist, which Mount decides before any store is touched.
		space.NewHandler(nil).Mount(r)
		page.NewHandler(nil, nil).Mount(r)
	})

	var got []string
	err := chi.Walk(r, func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		got = append(got, method+" "+strings.TrimSuffix(route, "/"))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(got)

	want := []string{
		// Create takes the workspace in the BODY — NOT in the path.
		"POST /v1/spaces",

		// ⚠ WORKSPACE-SCOPED: the COLLECTION-LEVEL queries, and only those. They answer
		// "across this workspace, give me…", so the workspace is the subject of the request
		// and belongs in the path.
		"GET /v1/workspaces/{wsID}/spaces",
		"GET /v1/workspaces/{wsID}/pages/search",
		"GET /v1/workspaces/{wsID}/pages/stale",

		// ⚠ EVERYTHING BY ID IS TOP-LEVEL. The id already determines the tenant, so the path
		// carries no workspace and the handler scopes to the caller's memberships instead
		// (GetByIDInWorkspaces) — a foreign id is 404, not 403.
		"DELETE /v1/spaces/{spaceID}",
		"GET /v1/spaces/{spaceID}",
		"PATCH /v1/spaces/{spaceID}",
		"DELETE /v1/spaces/{spaceID}/pages/{pageID}",
		"GET /v1/spaces/{spaceID}/pages",
		"GET /v1/spaces/{spaceID}/pages/{pageID}",
		"PATCH /v1/spaces/{spaceID}/pages/{pageID}",
		"POST /v1/spaces/{spaceID}/pages",
		// ⚠ `/version-cost`, NOT `/versions/cost`. The line below mounts `/versions/{version}`, so
		// a sibling under that prefix would resolve only by chi's static-over-param precedence and
		// would degrade to a 400 BAD_VERSION rather than a 404 the day it did not. It serves the
		// reconciliation between what version history SHOWS and what the page has SPENT.
		"GET /v1/spaces/{spaceID}/pages/{pageID}/version-cost",
		"GET /v1/spaces/{spaceID}/pages/{pageID}/versions",
		"GET /v1/spaces/{spaceID}/pages/{pageID}/versions/{version}",
		"GET /v1/spaces/{spaceID}/pages/{pageID}/versions/{version}/diff/{other}",
		"POST /v1/spaces/{spaceID}/pages/{pageID}/verify",
		"POST /v1/spaces/{spaceID}/pages/{pageID}/versions/{version}/restore",
	}
	sort.Strings(want)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the space/page route table changed.\n\ngot:\n  %s\n\nwant:\n  %s\n\n"+
			"⚠ IF THIS IS DELIBERATE, the suite's BFF addresses these paths directly "+
			"(apps/bff/lens.go, pinned by apps/bff/docs_routeshape_test.go). A path removed or "+
			"re-prefixed here becomes `404 page not found` there, and the suite renders that as "+
			"\"Docs is not configured\". Update both, in that order.",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestOnlyCollectionQueriesAreWorkspaceScoped states the RULE on its own, so it survives
// someone regenerating the table above from whatever happens to be registered.
//
// ⚠ I GOT THIS WRONG WRITING IT, WHICH IS THE ARGUMENT FOR WALKING THE ROUTER. My first
// version asserted "only the space list is workspace-scoped" — read off the routes the BFF
// happens to call. The router actually has three, because page SEARCH and STALE are
// collection queries too. Transcribing a table from the call sites you know about reproduces
// the original bug in the test that is supposed to catch it.
func TestOnlyCollectionQueriesAreWorkspaceScoped(t *testing.T) {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		space.NewHandler(nil).Mount(r)
		page.NewHandler(nil, nil).Mount(r)
	})

	var scoped []string
	_ = chi.Walk(r, func(method string, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.HasPrefix(route, "/v1/workspaces/") {
			scoped = append(scoped, method+" "+strings.TrimSuffix(route, "/"))
		}
		return nil
	})

	sort.Strings(scoped)
	want := []string{
		"GET /v1/workspaces/{wsID}/pages/search",
		"GET /v1/workspaces/{wsID}/pages/stale",
		"GET /v1/workspaces/{wsID}/spaces",
	}
	if strings.Join(scoped, "\n") != strings.Join(want, "\n") {
		t.Errorf("workspace-scoped routes = %v, want %v.\n\nTHE RULE: a collection query across "+
			"a workspace carries it in the path; anything addressed BY ID does not, because the id "+
			"already determines the tenant. A new one here is not wrong — but a caller that infers "+
			"the prefix from the routes it knows and applies it to the rest is exactly how five "+
			"Docs routes came to 404 in production.", scoped, want)
	}
}
