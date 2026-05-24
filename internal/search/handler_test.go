package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

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
