package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
