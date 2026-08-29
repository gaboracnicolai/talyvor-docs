package search

// SEMANTIC SEARCH'S ONLY TENANCY SCOPE IS ONE SQL PREDICATE, AND THE GO FILTER BELOW IT CANNOT
// NARROW TO A WORKSPACE.
//
// tab-5r8k's predicate census (#186) scored `semantic.go:287` — `WHERE p.workspace_id = $2` in
// `SemanticSearch.Search` — SILENT: rewrite it to `(p.workspace_id = $2 OR TRUE)` and the whole
// Go suite stays green. Its handover named this site and `:347` as the sharpest LOAD-BEARING
// candidates and said so with an explicit caveat: "`search/handler.go#visibleTo` re-filters, so
// measure before believing".
//
// MEASURED. `visibleTo` (handler.go, cited by SYMBOL — the line number that stood here, 349, was
// already 18 lines stale before the edit that moved it again) re-filters every row with
// `AuthorizePageRead`, and that
// is the SAME workspace-agnostic gate that made `analytics.GetWorkspaceStats`' predicates
// load-bearing in #187. Its own doc comment: "resolved against every workspace the caller belongs
// to. It cannot express which ONE of those workspaces the page is in, so a caller who is a member
// of two workspaces satisfies it for a page in either." So `visibleTo` does NOT re-establish the
// tenancy of a search keyed on `{wsID}`: it drops pages the caller may not read, and a page in
// the caller's OTHER workspace is not one of those.
//
// ⚠ THE RULE THAT MADE THIS DECIDABLE IN ONE READ, worth stating because 26 SILENT sites are
// still unsorted: A SILENT TENANCY PREDICATE IS LOAD-BEARING EXACTLY WHEN THE GO FILTER BELOW IT
// IS `AuthorizePageRead`, because that gate answers about the CALLER and not about the WORKSPACE.
// It is a grep for the gate, not a 50-minute census re-run.
//
// ⚠ AND THE CENSUS'S OTHER NAMED SITE IS NOT THE SAME KIND OF THING — MEASURED, AND DELIBERATELY
// NOT GUARDED HERE. `semantic.go:347` is inside `IndexAllPages`, and `IndexAllPages` HAS NO
// CALLER IN THIS REPOSITORY: not a route, not a boot path, not another package — `page/store.go`
// says so itself ("the documented boot backfill, has no caller in this repo"). Its predicate is
// SILENT because the function is unreached, which is a different fact from this file's site being
// SILENT because no fixture could express the caller. A guard over dead code protects nothing and
// would report coverage this repository does not have. The census's one verdict, SILENT, covers
// both cases and cannot tell them apart; that is the limit of the instrument, recorded rather
// than papered over.
//
// ⚠ WHY NOTHING SAW IT: every existing search fixture builds its caller with exactly ONE
// membership (`authz_test.go:34`, `handler_test.go:35`, `privatespace_realpg_test.go:86`,
// `mergepaging`, `offset`, `cost`, `search_realpg` — all of them). The two-workspace caller has
// never been executed against this endpoint, so the disagreeing case was unobservable BY
// CONSTRUCTION rather than overlooked, and the census found 0 tests rather than a weak one.
//
// TEST-ONLY: the predicate is CORRECT today. What was missing was anything that would notice if
// it stopped being.
//
//	[PREMISE-FOREIGN-READABLE] carol's search of wsB DOES return wsB's page ← the load-bearing premise
//	[OWN-PRESENT]              carol's search of wsA DOES return wsA's page ← the floor; refuses an empty pass
//	[FOREIGN-ABSENT]           carol's search of wsA does NOT return wsB's  ← semantic.go:287

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
	"github.com/talyvor/docs/internal/permission"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/spaceauth"
	"github.com/talyvor/docs/internal/testutil"
)

func TestSemanticSearch_ForeignWorkspaceIsNotSearched_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	wsA := d.Workspace(t)
	wsB := d.Workspace(t)
	const carol = "carol@example.com"
	carolA := d.Member(t, wsA, carol)
	carolB := d.Member(t, wsB, carol)

	// Both spaces are PUBLIC on purpose. A private wsB space would be dropped by the page gate
	// and this test would prove nothing about the SQL — the same trap #187's roll-up guard
	// documents. Neither page's title or body contains the query term, so the full-text half
	// cannot fire and every row below is a PURE-SEMANTIC one.
	const ownTitle = "A Deployment Runbook"
	const foreignTitle = "B Acquisition Runbook"
	spA := seedSpace(t, d, wsA, carolA, "Team A Ops", false)
	pgA := seedPage(t, d, wsA, spA, carolA, ownTitle, "the deployment runbook for team a")
	spB := seedSpace(t, d, wsB, carolB, "Team B Ops", false)
	pgB := seedPage(t, d, wsB, spB, carolB, foreignTitle, "the acquisition runbook for team b")

	// The SAME vector for both pages AND for the query, so every cosine similarity is 1.0 and the
	// 0.75 threshold cannot be what removes a row. If wsB's page is absent below it is because of
	// the WHERE clause and nothing else.
	vec := unitVector()
	for _, id := range []string{pgA, pgB} {
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO page_embeddings (page_id, embedding) VALUES ($1, $2::vector)`,
			id, vec); err != nil {
			t.Fatalf("seed embedding for %s: %v", id, err)
		}
	}

	lens := newFakeLens(t)
	defer lens.Close()
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vec + `}]}`))
	}

	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	// THE REAL GATE, not permitAll. permitAll would make [PREMISE-FOREIGN-READABLE] trivially
	// true and hide the very thing being measured — that the SHIPPED authorizer says yes to a
	// page in the caller's other workspace.
	h := NewHandler(page.NewStore(d.Pool), sem).WithAccess(
		spaceauth.New(space.NewStore(d.Pool), permission.NewStore(d.Pool)).
			WithPageMeta(pageMetaLooker(d)))

	// carol carries BOTH memberships, exactly as authz.Middleware would leave them. This is the
	// caller no other fixture in this package can express.
	searchWS := func(t *testing.T, wsID string) []Result {
		t.Helper()
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(), carol,
					[]authz.Membership{
						{WorkspaceID: wsA, MemberID: carolA},
						{WorkspaceID: wsB, MemberID: carolB},
					})))
			})
		})
		r.Route("/v1", func(r chi.Router) { h.Mount(r) })
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+wsID+"/search?q=rollback", nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET search of %s as carol = HTTP %d: %s", wsID, rr.Code, rr.Body.String())
		}
		var body response
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, rr.Body.String())
		}
		return body.Results
	}

	hasPage := func(rs []Result, id string) bool {
		for _, r := range rs {
			if r.PageID == id {
				return true
			}
		}
		return false
	}

	// ── [PREMISE-FOREIGN-READABLE] THE LOAD-BEARING PREMISE, MEASURED THROUGH THE SAME ENDPOINT.
	// Searching wsB returns wsB's page, so the shipped page-read gate says YES to it for THIS
	// caller. Without this, [FOREIGN-ABSENT] below could hold because `visibleTo` filtered the
	// row — a pass for a reason that has nothing to do with the workspace predicate.
	if got := searchWS(t, wsB); !hasPage(got, pgB) {
		t.Fatalf("[PREMISE-FOREIGN-READABLE] searching wsB as carol did not return wsB's page %s "+
			"(got %d rows) — if the page gate or the semantic half refuses this row, every "+
			"absence assertion below is vacuous", pgB, len(got))
	}

	rows := searchWS(t, wsA)

	// ── [OWN-PRESENT] The floor. An empty result satisfies [FOREIGN-ABSENT] perfectly, so the
	// wsA page must be PRESENT — this is what refuses "the semantic half returned nothing" as a
	// clean run.
	if !hasPage(rows, pgA) {
		t.Errorf("[OWN-PRESENT] searching wsA as carol did not return wsA's own page %s (got %d "+
			"rows) — the absence assertion below cannot mean anything against an empty result",
			pgA, len(rows))
	}

	// ── [FOREIGN-ABSENT] The defect semantic.go:287 prevents. A pure-semantic row carries the
	// page id and a similarity, and since #90/#91 it carries the TITLE and SPACE NAME too — so a
	// leaked row here names the other tenant's document and links to it.
	if hasPage(rows, pgB) {
		var leaked string
		for _, r := range rows {
			if r.PageID == pgB {
				b, _ := json.Marshal(r)
				leaked = string(b)
			}
		}
		t.Errorf("[FOREIGN-ABSENT] searching workspace %s as carol returned a page from workspace "+
			"%s: %s", wsA, wsB, leaked)
	}
}
