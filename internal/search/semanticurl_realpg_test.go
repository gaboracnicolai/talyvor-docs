package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// A SEMANTIC-ONLY SEARCH HIT WAS THE ONE RESULT YOU COULD NOT OPEN.
//
// merge() built pure-semantic rows with `pageURL("", s.PageID)` — which is `/pages/{id}`, because
// pageURL branches on an empty spaceID — and the SPA HAS NO SUCH ROUTE. `router/paths.ts`
// registers exactly `/spaces/:spaceID`, `/spaces/:spaceID/pages/:pageID` and `/s/:token`, so
// `/pages/{id}` falls to `*` = NotFoundView. The click path is `SearchModal.tsx spaceIDFromURL`
// (it matches `^/spaces/([^/]+)/pages/`, so it returns **undefined** for that shape) ->
// `router/Layout.tsx` `navigate(r.spaceID ? paths.page(...) : r.url)` -> the fallback fires ->
// Not found. The entire point of the semantic half is to find by MEANING what full-text missed,
// and every row only it could produce was a dead link.
//
// ⚠ THE COMMENTS AT BOTH ENDS ASSERTED THE OPPOSITE. Layout.tsx said the fallback uses "the
// result's own url (already a /spaces/../pages/.. string)" — false for exactly the one case the
// fallback exists to serve — and SearchModal.tsx said the caller "routes to a bare page view",
// which does not exist in the route table.
//
// ⚠⚠ AND THE REASON IT READ AS A COST DECISION IS A MEASURABLY FALSE PREMISE. The handler's own
// doc says a semantic row is "built from a vector hit with no pages row read", so filling anything
// in reads as buying a query. For the SPACE that is simply untrue: SemanticSearch.Search ALREADY
// does `JOIN pages p ON p.id = pe.page_id` and ALREADY filters `p.space_id = $4` in the same
// statement. The column was in the join and absent from the SELECT list. Nothing is bought here,
// and no access model changes — which is why this is a fix and not a decision.
//
// THIS TEST DRIVES THE SHIPPED CHAIN ON REAL POSTGRES — the real page store as the full-text half,
// the real SemanticSearch over real pgvector, the real merge, behind a page-read gate — and
// asserts the one thing the defect gets wrong: the URL a semantic-only row hands the router.
//
// ⚠ IT IS ALSO THE SECOND TEST IN THE REPO THAT MAKES POSTGRES COMPILE THE pgvector QUERY (see
// spacefilter_realpg_test.go's note). Every other semantic test drives pgxmock, which regex-matches
// SQL text and never asks a database to plan it — so a SELECT list that does not compile is
// invisible to them by construction, and this change edits exactly that SELECT list.
func TestSearch_RealPG_SemanticOnlyHitCarriesTheSpaceItIsAlreadyJoinedOn(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	space := seedSpace(t, d, ws, alice, "Alpha Space", false)
	// The body deliberately does not contain the query term, so the full-text half cannot fire and
	// the row below is a PURE-SEMANTIC one. That is asserted, not assumed — see [SRC].
	pageID := seedPage(t, d, ws, space, alice, "Alpha Runbook", "the deployment runbook")

	vec := unitVector()
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO page_embeddings (page_id, embedding) VALUES ($1, $2::vector)`,
		pageID, vec); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}

	lens := newFakeLens(t)
	defer lens.Close()
	// The fake returns the SAME vector that is stored, so cosine similarity is 1.0 — over
	// similarityThreshold (0.75) whatever the query text is.
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vec + `}]}`))
	}

	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	h := NewHandler(page.NewStore(d.Pool), sem).WithAccess(permitAll{})
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(),
				"alice@example.com", []authz.Membership{{WorkspaceID: ws, MemberID: alice}})))
		})
	})
	r.Route("/v1", func(r chi.Router) { h.Mount(r) })

	// `type` is omitted on purpose: "all" is the default, and it is what SearchModal sends.
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/search?q=rollback", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /search = HTTP %d %s", rr.Code, rr.Body.String())
	}
	var body response
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	// [PREMISE-COUNT] is the FLOOR, and it is the only assertion here that can speak when the
	// semantic half returns nothing at all — [SRC] and [URL] never evaluate on an empty result, so
	// without it a query that stopped matching would read as a clean run.
	if len(body.Results) != 1 {
		t.Fatalf("[PREMISE-COUNT] want exactly 1 result, got %d (%s) — the semantic half did not "+
			"fire, so nothing below is a measurement of a semantic-only row",
			len(body.Results), rr.Body.String())
	}
	got := body.Results[0]
	// ⚠ A `got.PageID != pageID` PREMISE CHECK STOOD HERE AND IS DELETED. No mutation justifies it:
	// [URL] pins the whole string INCLUDING the seeded page id, so every state that check could
	// fire on reddens [URL] too. It read as a third layer of coverage and was a restatement.
	// [SRC] is the premise of the whole test. "both"/"fulltext" would mean the FULL-TEXT half
	// matched and the URL came from the pages row it read — a different code path from the broken
	// one, and an assertion on it would say nothing about semantic-only rows.
	if got.Source != "semantic" {
		t.Fatalf("[SRC] source = %q, want \"semantic\" — the full-text half matched, so this row is "+
			"not the pure-semantic shape the defect lives on", got.Source)
	}

	// [URL] is the assertion the defect fails. `/pages/{id}` resolves to `*` = NotFoundView in the
	// SPA's route table; `/spaces/{s}/pages/{p}` is the page route.
	want := "/spaces/" + space + "/pages/" + pageID
	if got.URL != want {
		t.Errorf("[URL] a semantic-only hit's url = %q, want %q. The SPA has no /pages/:id route, so "+
			"this row navigates to Not found — the one result full-text could not have produced is "+
			"the one result the reader cannot open.", got.URL, want)
	}
	// ⚠ THERE WAS A SECOND ASSERTION HERE AND IT IS DELETED RATHER THAN KEPT. It checked
	// `strings.HasPrefix(got.URL, "/spaces/")` — the prefix SearchModal's `^/spaces/([^/]+)/pages/`
	// extraction keys on. It is STRICTLY WEAKER than [URL] above: every mutation it can see, [URL]
	// sees first, so no control could ever justify it and a passing run said nothing about it. Two
	// assertions where one does the work reads as twice the coverage.
}
