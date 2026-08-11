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

// A SEMANTIC-ONLY SEARCH HIT RENDERS AS A BLANK ROW.
//
// `SearchModal.tsx` draws a result's identity as exactly two spans separated by a dot —
// `{r.space_name} · {r.page_title}` — with `r.headline` beneath. `merge()` fills NONE of the three
// on a pure-semantic row (handler.go, the `for _, s := range sem` branch sets PageID, Similarity,
// Source and URL and nothing else), so the row the reader sees is an icon, a "·", and empty space.
//
// ⚠ IT IS THE SAME SHAPE #90 CLOSED, ONE FIELD ALONG. That merge made the row OPENABLE (it was
// built with `pageURL("", …)` = `/pages/{id}`, a route the SPA does not register). A row you can
// open and cannot read is the same defect in the same place: the whole reason the semantic half
// exists is to find by MEANING what full-text missed, and every row only it can produce is
// unreadable in the list.
//
// ⚠ WHY IT IS A PLUMBING FIX AND NOT A DECISION, MEASURED IN THE QUERY'S OWN TEXT — the same
// argument #90 made for `space_id`, and this file is the second half of it. `SemanticSearch.Search`
// is `FROM page_embeddings pe JOIN pages p ON p.id = pe.page_id`, filtering `p.workspace_id`,
// `p.is_template` and `p.space_id` on that row: **`title` is a column of a row the statement
// already reads**, absent only from the SELECT list. `space_name` needs one more join — the
// IDENTICAL join `page.Store.SearchWithRank` already performs for the full-text half
// (`JOIN spaces sp ON sp.id = p.space_id`, store.go) — and it cannot drop a row, because
// `0002_pages.sql:13` declares `space_id TEXT NOT NULL REFERENCES spaces(id)`. Nothing here buys a
// per-result lookup and no access model changes.
//
// ⚠ WHAT IS DELIBERATELY STILL ABSENT, so the next reader does not take this merge as the whole of
// finding (3). `headline` is `ts_headline` over the FULL-TEXT tsquery — a semantic hit is by
// definition a document that query did not match, so what that call returns on this row is a
// product question and a real computation, not a column. The THREE COST FIELDS stay nil for the
// reason already written on `Result`: whether a number that has never been rendered on this row
// should start appearing is a question about a MONEY surface, and it is not settled by discovering
// it would be cheap. This merge changes neither.
//
// ⚠ NOTHING IN THE SUITE COULD SEE IT, AND THE FRONTEND FIXTURE IS WHY. `SearchModal.cost.test.tsx`
// builds its semantic row as `{page_title: "Semantic", space_name: "Ops", …}` — a wire shape the
// server has never sent. A fixture more generous than the wire makes the renderer's two spans
// unfalsifiable, so the blank row could not be red in the frontend either.
//
// THIS TEST DRIVES THE SHIPPED CHAIN ON REAL POSTGRES — the real page store as the full-text half,
// the real SemanticSearch over real pgvector, the real merge, behind a page-read gate. It is a
// separate file from semanticurl_realpg_test.go on purpose: that test's evidence was gathered for
// the URL, and reusing its function would lend this assertion provenance it never earned.
//
// ⚠ AND IT IS ONE OF ONLY THREE TESTS IN THE REPO THAT MAKE POSTGRES COMPILE THE pgvector QUERY.
// Every other semantic test drives pgxmock, which regex-matches SQL text and never asks a database
// to plan it — so a SELECT list or a JOIN that does not compile is invisible to them BY
// CONSTRUCTION, and this change edits exactly those two clauses.
func TestSearch_RealPG_SemanticOnlyHitCarriesTheTitleItsOwnQueryAlreadyReads(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	// The two strings are DIFFERENT so a fix that fills one field from the other's value is caught
	// rather than accepted: [TITLE] and [SPACE-NAME] pin distinct bytes.
	const spaceName = "Alpha Space"
	const pageTitle = "Alpha Runbook"
	space := seedSpace(t, d, ws, alice, spaceName, false)
	// Neither the title nor the body contains the query term, so the full-text half cannot fire
	// and the row below is a PURE-SEMANTIC one. That is asserted, not assumed — see [SRC].
	pageID := seedPage(t, d, ws, space, alice, pageTitle, "the deployment runbook")

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
	// semantic half returns nothing at all — every assertion below reads body.Results[0] and
	// never evaluates on an empty result, so without it a query that stopped matching would read
	// as a clean run.
	if len(body.Results) != 1 {
		t.Fatalf("[PREMISE-COUNT] want exactly 1 result, got %d (%s) — the semantic half did not "+
			"fire, so nothing below is a measurement of a semantic-only row",
			len(body.Results), rr.Body.String())
	}
	got := body.Results[0]
	// [SRC] is the premise of the whole test. "both"/"fulltext" would mean the FULL-TEXT half
	// matched and these fields came from the pages row IT read — a different branch of merge()
	// from the broken one, and an assertion on it would say nothing about semantic-only rows.
	if got.Source != "semantic" {
		t.Fatalf("[SRC] source = %q, want \"semantic\" — the full-text half matched, so this row is "+
			"not the pure-semantic shape the defect lives on", got.Source)
	}

	// [TITLE] and [SPACE-NAME] are the two spans SearchModal draws as the row's identity line.
	// Both are t.Errorf, not t.Fatalf: they fail independently on the unmodified tree and each
	// earns its own mutation (the SELECT list drops p.title; the spaces join is removed).
	if got.PageTitle != pageTitle {
		t.Errorf("[TITLE] a semantic-only hit's page_title = %q, want %q. SearchModal renders this "+
			"as the row's name — empty, the reader is offered a result with nothing written on it. "+
			"`title` is a column of the pages row SemanticSearch.Search already joins.",
			got.PageTitle, pageTitle)
	}
	if got.SpaceName != spaceName {
		t.Errorf("[SPACE-NAME] a semantic-only hit's space_name = %q, want %q. It is the other span "+
			"on the row's identity line, and the full-text half already reads it through the "+
			"IDENTICAL `JOIN spaces sp ON sp.id = p.space_id` (page.Store.SearchWithRank).",
			got.SpaceName, spaceName)
	}
}
