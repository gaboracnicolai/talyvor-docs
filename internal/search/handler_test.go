package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
)

type fakePages struct {
	results []page.SearchResult
	called  bool
}

func (f *fakePages) SearchWithRank(_ context.Context, _, _ string, _ *string, _, _ int) ([]page.SearchResult, error) {
	f.called = true
	return f.results, nil
}

func newRouter(t *testing.T, pages fullTextSearcher, sem *SemanticSearch) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	// Mirror production: authz stamps verified memberships before handlers. These tests call as a
	// member of ws-1 (the workspace in the URL).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authz.WithMemberships(req.Context(), "u@ws1.com", []authz.Membership{{WorkspaceID: "ws-1", MemberID: "m"}})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/v1", func(r chi.Router) {
		NewHandler(pages, sem).Mount(r)
	})
	return r
}

func TestHandler_RejectsShortQuery(t *testing.T) {
	h := newRouter(t, &fakePages{}, &SemanticSearch{})
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?q=a", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestHandler_FullTextOnly_NoLens(t *testing.T) {
	pages := &fakePages{
		results: []page.SearchResult{
			{
				Page:      model.Page{ID: "pg-1", Title: "Auth flow", SpaceID: "sp-1"},
				SpaceName: "Engineering",
				Rank:      0.9,
				Headline:  "Some <mark>auth</mark> excerpt",
			},
		},
	}
	// Empty SemanticSearch with no Lens — Search returns [], no error.
	sem := newSemanticSearch(lensintegration.New("", ""), nil)
	h := newRouter(t, pages, sem)

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?q=auth+flow", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp response
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result, got %d", len(resp.Results))
	}
	if resp.Results[0].Source != "fulltext" {
		t.Fatalf("expected source=fulltext, got %q", resp.Results[0].Source)
	}
	if resp.Results[0].URL != "/spaces/sp-1/pages/pg-1" {
		t.Fatalf("url not built: %q", resp.Results[0].URL)
	}
	if resp.Total != 1 || resp.Query != "auth flow" {
		t.Fatalf("metadata wrong: %+v", resp)
	}
}

// The handler surfaces the search-side fail-closed policy: when the semantic side cannot mint
// a per-workspace token, the whole search errors (500) rather than falling back to the shared
// global key. type=all here — full-text would succeed, but fail-closed erases it too, by design.
func TestHandler_FailsClosedWhenTokenMintFails(t *testing.T) {
	f := newJWTFakeLens(t)
	f.mintFail = true
	defer f.Close()
	sem, _ := meteredSemantic(t, f.URL)
	pages := &fakePages{results: []page.SearchResult{
		{Page: model.Page{ID: "pg-1", Title: "Auth", SpaceID: "sp-1"}, SpaceName: "Eng", Rank: 0.9, Headline: "h"},
	}}
	h := newRouter(t, pages, sem)

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?q=auth&type=all", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 (fail-closed) on mint failure, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, _, dataAuth, _ := f.snapshot(); len(dataAuth) != 0 {
		t.Fatalf("a Lens data-path request went out despite the mint failure: %v", dataAuth)
	}
}

func TestMerge_MarksDuplicatesAsBoth(t *testing.T) {
	ft := []page.SearchResult{
		{Page: model.Page{ID: "pg-1", Title: "A", SpaceID: "sp-1"}, SpaceName: "Eng", Rank: 0.9, Headline: "h"},
		{Page: model.Page{ID: "pg-2", Title: "B", SpaceID: "sp-1"}, SpaceName: "Eng", Rank: 0.5, Headline: "h"},
	}
	sem := []SemanticResult{
		{PageID: "pg-1", Similarity: 0.82},
		{PageID: "pg-3", Similarity: 0.9}, // semantic-only
	}
	out := merge(ft, sem)
	if len(out) != 3 {
		t.Fatalf("want 3 results, got %d", len(out))
	}
	// pg-1 is in both sets → Source=both.
	var hitBoth, hitSemanticOnly bool
	for _, r := range out {
		if r.PageID == "pg-1" && r.Source == "both" {
			hitBoth = true
		}
		if r.PageID == "pg-3" && r.Source == "semantic" {
			hitSemanticOnly = true
		}
	}
	if !hitBoth {
		t.Fatalf("expected pg-1 marked source=both: %+v", out)
	}
	if !hitSemanticOnly {
		t.Fatalf("expected pg-3 marked source=semantic: %+v", out)
	}
}

// ⚠ AN UNRECOGNISED `type` RAN NEITHER HALF AND ANSWERED 200 WITH AN EMPTY LIST — BYTE-IDENTICAL
// TO A WORKSPACE WITH NO MATCHING DOCUMENTS.
//
// The handler read `kind := r.URL.Query().Get("type")`, defaulted an EMPTY value to `all`, and then
// tested `kind == "all" || kind == "fulltext"` and `kind == "all" || kind == "semantic"`. Anything
// else — a typo, a rename, `Semantic` with a capital, or a trailing space (`q` is TrimSpace'd,
// `kind` is not) — matched neither arm. Both halves were skipped, `results` stayed empty, and the
// envelope went out as a 200.
//
// ⚠ THE REPAIR WAS NAMED BY THE SESSION THAT FOUND IT AND COULD NOT TAKE IT. talyvor-suite's W1.7
// handover records: "THIS IS ALREADY MITIGATED FOR BROWSER CALLERS ... apps/bff/docs_search.go
// refuses an unrecognised type with a 400, so it is NOT a live defect on this deployment — it is
// exposure for any OTHER caller of Docs, and the honest repair is upstream refusal." It was handed
// on because this repository was held by another tab at the time.
//
// ⚠ THE CLOSED SET IS NOT INVENTED HERE. apps/bff already refuses anything outside
// {all, fulltext, semantic} and its refusal message names exactly those three, so this makes the
// upstream agree with a set the product had already decided. Checked read-only before the change:
// no caller in this repo, its frontend, or the suite sends a type outside that set.
//
// ⚠ AND THE DEFAULT IS NOT A MEMBER OF THE SET. An ABSENT `type` means `all` and must keep meaning
// it — refusing it would break every existing caller, and the empty string is exactly the value a
// naive "is it in the map" check gets wrong. Asserted below, first.
func TestSearch_AnUnrecognisedTypeIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	hit := []page.SearchResult{{
		Page:      model.Page{ID: "pg-1", Title: "Auth flow", SpaceID: "sp-1"},
		SpaceName: "Engineering",
	}}

	drive := func(t *testing.T, query string) (int, string, bool) {
		t.Helper()
		pages := &fakePages{results: hit}
		h := newRouter(t, pages, &SemanticSearch{})
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?"+query, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code, rr.Body.String(), pages.called
	}

	// ⚠ THE MUST-STAY-GREEN HALF, AND IT RUNS FIRST. A refusal that also refuses the three real
	// values, or the absent one, is not a repair.
	//
	// ⚠⚠ AND IT ASSERTS THE FIXTURE IS LIVE, WHICH THE FIRST VERSION OF THIS TEST DID NOT.
	// Control Q5 emptied `hit` — so the store holds NO matching document — and NOTHING reddened.
	// Every assertion was about status codes, and the refusal fires before the store is consulted,
	// so the matching row existed only in the failure MESSAGE. The sentence this test exists to
	// prove is "a match was sitting there and the caller was told nothing matched"; a fixture that
	// can be emptied without a red does not prove it. The `all`/`fulltext` arms now require the
	// store to have been READ and its row to be ON SCREEN.
	for _, ok := range []string{"q=auth", "q=auth&type=all", "q=auth&type=fulltext", "q=auth&type=semantic"} {
		code, body, storeCalled := drive(t, ok)
		if code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200. This is a value the product sends today — an absent "+
				"type means `all` and the three named types are apps/bff's own closed set.", ok, code)
		}
		// type=semantic deliberately does NOT read the full-text store, so it is excluded here
		// rather than weakening the rule for the other three.
		if ok == "q=auth&type=semantic" {
			continue
		}
		if !storeCalled {
			t.Fatalf("%s: the full-text store was never read on a type that runs it. THE FIXTURE "+
				"IS NOT LIVE, and every 'a match was sitting there' claim below is then decoration.", ok)
		}
		if !strings.Contains(body, "pg-1") {
			t.Fatalf("%s: the matching page is not in the answer (%s). The contrast this test "+
				"draws — a real hit against the empty answer an unknown type produces — needs the "+
				"hit to actually be there.", ok, body)
		}
	}

	// ⚠ THE FINDING. Each of these matched neither arm and produced a 200 with an empty list.
	// `Semantic` and `semantic ` are in the list deliberately: the first is the rename/casing
	// case, the second is the one `q`'s TrimSpace hides — `kind` was never trimmed.
	for _, bad := range []string{"type=banana", "type=Semantic", "type=semantic%20", "type=fulltxt"} {
		code, body, storeCalled := drive(t, "q=auth&"+bad)
		if code == http.StatusOK {
			t.Errorf("q=auth&%s: answered %d %s (full-text store called: %v).\n"+
				"NEITHER HALF RAN AND THE CALLER CANNOT TELL. The store holds a matching document "+
				"and this answer is byte-identical to a workspace that has none — a caller that "+
				"renamed a type, capitalised it, or left a trailing space reads 'nothing matched' "+
				"forever. Refuse it: apps/bff already does, on the same three values.",
				bad, code, body, storeCalled)
			continue
		}
		if code != http.StatusBadRequest {
			t.Errorf("q=auth&%s: got %d, want 400", bad, code)
		}
		// ⚠ "NEITHER HALF RAN" IS THE FINDING AND IT IS ASSERTED, NOT NARRATED. A refusal that
		// still went to the store would be a different (and cheaper-to-get-wrong) thing.
		if storeCalled {
			t.Errorf("q=auth&%s: refused with %d but read the full-text store first. A refusal "+
				"issued after the work is not a refusal.", bad, code)
		}
	}
}
