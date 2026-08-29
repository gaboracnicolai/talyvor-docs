package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
)

// THE SEARCH ROUTE'S TWO REMAINING PAGING BOUNDS, NEITHER OF WHICH ANYTHING NOTICED.
//
// W3.53's census left these UNTESTED and W3.54 argued against covering them — "a fifth paging test
// adds rows and not information once F9 pins the window they all size themselves from". That
// argument was written BEFORE F9 was pinned. RE-MEASURED after `d0788f2` pinned it: F9 covered
// NEITHER of these, and each is the sole control of a DIFFERENT observable.
//
//	limit clamp removed (?type=fulltext&limit=1000) -> store asked for 1000 instead of 50,
//	                                                   and the caller receives 60 rows, not 50
//	fetchLimit bound removed (?limit=20)            -> store asked for 80 instead of 50;
//	                                                   with limit=1000, 200 instead of 50
//
// ⚠ THE TWO ARE OBSERVABLE IN DIFFERENT PLACES AND THAT IS WHY BOTH ARE HERE. The limit clamp
// changes what the CALLER RECEIVES. The fetchLimit bound changes only what the STORE IS ASKED FOR
// — `page.Store.SearchWithRank` clamps to 50 on its own account, so no extra row ever comes back
// and the response is identical. It bounds QUERY COST, not answer size, and a response-level
// assertion would call it unreachable. Its test asserts the argument for exactly that reason.
//
// ⚠ THE ACCESS GATE MUST BE WIRED TO SEE THE FETCHLIMIT BOUND AT ALL, and this is not boilerplate:
// with `h.access == nil` the handler takes the `fetchLimit = window` branch, which SKIPS the
// over-fetch multiply entirely. Measured — the first probe run reported the bound unobservable for
// that reason, and it was the fixture, not the bound.
//
// ⚠ NOT COVERED HERE, DELIBERATELY, AND IT IS A TRUE ROW RATHER THAN AN OMISSION:
// `SemanticSearch.Search`'s own `limit > 50` clamp. Its ONLY caller is this handler, which hands it
// `fetchLimit` — already bounded to 50 by the bound above. So it CANNOT fire unless that bound
// fails first: pure defence in depth, reachable only through another bug. W4.44's rule — a bound
// nothing can reach is a true row and not a finding.

// countingPages records the limit it was asked for and returns `rows` results, so a truncation to
// the caller's limit is observable. Sixty is deliberate: with fewer rows than the clamp's own value
// nothing downstream can tell 50 from 1000.
type countingPages struct {
	limit, offset, calls int
	rows                 int
}

func (c *countingPages) SearchWithRank(_ context.Context, _, _ string, _ *string, l, off int) ([]page.SearchResult, error) {
	c.calls++
	c.limit, c.offset = l, off
	out := make([]page.SearchResult, c.rows)
	for i := range out {
		out[i].Page.ID = fmt.Sprintf("pg-%03d", i)
		out[i].Page.SpaceID = "sp-1"
		out[i].Rank = float64(c.rows - i)
	}
	return out, nil
}

// gatedRouter mounts the handler WITH an access gate, which is the production wiring and the only
// one in which the over-fetch multiply runs at all.
func gatedRouter(t *testing.T, pages fullTextSearcher) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authz.WithMemberships(req.Context(), "u@ws1.com",
				[]authz.Membership{{WorkspaceID: "ws-1", MemberID: "m"}})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/v1", func(r chi.Router) {
		NewHandler(pages, newSemanticSearch(lensintegration.New("", ""), nil)).
			WithAccess(permitAll{}).Mount(r)
	})
	return r
}

func askSearch(t *testing.T, rows int, query string) (askedLimit, gotRows int) {
	t.Helper()
	cp := &countingPages{rows: rows}
	h := gatedRouter(t, cp)
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?q=auth+flow&"+query, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", query, rr.Code, rr.Body.String())
	}
	// NON-VACUITY. Every assertion below reads a number off this stub; if the handler never called
	// it, all of them would be reading the zero value and passing.
	if cp.calls != 1 {
		t.Fatalf("%s: store called %d times, want exactly 1", query, cp.calls)
	}
	var resp response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: decode: %v (%s)", query, err, rr.Body.String())
	}
	return cp.limit, len(resp.Results)
}

func TestSearch_LimitClamp_BoundsWhatTheCallerReceives(t *testing.T) {
	const corpus = 60 // more than the clamp, so 50 and 1000 are distinguishable

	// COUNTERWEIGHT FIRST: a clamp that pinned every limit to 50 would satisfy the over-the-bound
	// cases below. A limit under the ceiling must still reach the caller unchanged.
	if _, rows := askSearch(t, corpus, "type=fulltext&limit=10"); rows != 10 {
		t.Fatalf("limit=10 returned %d rows, want 10 — the clamp must not pin a small page", rows)
	}
	// EXACTLY AT THE BOUND IS ADMITTED WHOLE, not cut to 49.
	if _, rows := askSearch(t, corpus, "type=fulltext&limit=50"); rows != 50 {
		t.Fatalf("limit=50 returned %d rows, want 50 — the value AT the ceiling must be admitted", rows)
	}
	// ⚠ THIS TEST ASSERTS THE RESPONSE AND DELIBERATELY SAYS NOTHING ABOUT THE STORE ARGUMENT,
	// which is the fetchLimit bound's business and is asserted in the test below. An earlier draft
	// checked both here, and a control caught it: neutering the fetchLimit bound red BOTH tests, so
	// a red no longer said WHICH bound had gone. Overlapping assertions make a failure unreadable.
	for _, q := range []string{"type=fulltext&limit=51", "type=fulltext&limit=1000", "type=all&limit=1000"} {
		if _, rows := askSearch(t, corpus, q); rows != 50 {
			t.Fatalf("%s returned %d rows, want 50 — the caller's page size is bounded by the "+
				"limit clamp and by nothing else; with it gone the whole over-fetched set is "+
				"handed back", q, rows)
		}
	}
}

func TestSearch_FetchLimitBound_BoundsWhatTheStoreIsAskedFor(t *testing.T) {
	// This bound is invisible in the RESPONSE — page.Store.SearchWithRank clamps to 50 itself, so
	// no extra row ever comes back. It bounds the QUERY, which is why the assertion is on the
	// argument. A response-level test here would pass with the bound removed.
	for _, tc := range []struct {
		query string
		want  int
	}{
		// COUNTERWEIGHT: below the ceiling the over-fetch is passed through, so the bound is shown
		// not to be pinning every request to 50.
		{"type=all&limit=10&offset=0", 40}, // 10 x searchFetchFactor(4), under the 50 ceiling
		{"type=all&limit=12&offset=0", 48},
		// AT and OVER the ceiling.
		{"type=all&limit=13&offset=0", maxFetchRows}, // 13x4 = 52, the first request that must be cut
		{"type=all&limit=20&offset=0", maxFetchRows}, // would be 80
		{"type=all&limit=1000&offset=0", maxFetchRows},
	} {
		asked, _ := askSearch(t, 60, tc.query)
		if asked != tc.want {
			t.Fatalf("%s asked the store for %d, want %d — this is the only bound on how many rows "+
				"the over-fetch multiply may request, and the store's own clamp hides its absence "+
				"from every response-level assertion", tc.query, asked, tc.want)
		}
	}
}
