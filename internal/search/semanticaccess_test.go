package search

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
)

// emptyFullText is the full-text half returning nothing, so every row in the response below is a
// PURE-SEMANTIC one — the rows the private-space real-PG test cannot produce (it runs with
// type=fulltext because a Lens-less SemanticSearch answers []).
type emptyFullText struct{}

func (emptyFullText) SearchWithRank(context.Context, string, string, *string, int, int) ([]page.SearchResult, error) {
	return nil, nil
}

// denyList is a pageReader that refuses the ids it is given and permits everything else. Both
// arms matter: the permitted id is the positive control that keeps "the endpoint returned nothing"
// from reading as "the private row was filtered".
type denyList struct{ denied map[string]bool }

func (d denyList) AuthorizePageRead(_ context.Context, pageID string) (bool, bool) {
	if d.denied[pageID] {
		return true, false
	}
	return true, true
}

// THE SEMANTIC HALF LEAKS A DIFFERENT, SMALLER FACT — AND FROM THE SAME UNFILTERED POOL.
//
// A pure-semantic row carries no title and no headline (merge builds it from a vector hit with no
// pages row read), so what escapes is a page id, a URL and a cosine similarity. That is still an
// answer about a document the caller cannot open: "the private board memo is 0.93 relevant to
// 'layoffs'". SemanticSearch.Search's SQL scopes to `p.workspace_id` and `is_template` only —
// exactly the two filters the full-text side had.
//
// This drives the REAL handler over the REAL SemanticSearch (pgxmock pool + the package's fake
// Lens), so the assertion is about the response the route writes, not about visibleTo in isolation.
func TestSearch_SemanticOnlyRow_RespectsPageAccess(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	sem, pool := newSemantic(t, srv.URL)

	// 40, not 10: with an access gate wired the handler asks the store for maxFetchFactor×limit so
	// hidden rows do not become missing ones, and pgxmock matching on the argument is what pins
	// that the SEMANTIC half over-fetches too. (The Lens embed is one call either way — the window
	// is a pgvector LIMIT, so a wider one costs no extra spend.)
	pool.ExpectQuery(`page_embeddings.*<=>`).
		WithArgs(pgxmock.AnyArg(), "ws-1", 40, (*string)(nil), 0).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "space_id", "title", "space_name", "similarity"}).
			AddRow("private-memo", "sp-private", "Quarterly Layoffs", "Board Private", float64(0.93)).
			AddRow("public-handbook", "sp-public", "Employee Handbook", "Handbook", float64(0.88)))

	h := NewHandler(emptyFullText{}, sem).
		WithAccess(denyList{denied: map[string]bool{"private-memo": true}})

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c := authz.WithMemberships(req.Context(), "bob@example.com",
				[]authz.Membership{{WorkspaceID: "ws-1", MemberID: "m-bob"}})
			next.ServeHTTP(w, req.WithContext(c))
		})
	})
	r.Route("/v1", func(r chi.Router) { h.Mount(r) })

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?q=layoffs&type=semantic", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "public-handbook") {
		t.Fatalf("PREMISE FAILED: the permitted semantic row is absent too, so an absent private "+
			"row proves nothing: %s", body)
	}
	if strings.Contains(body, "private-memo") {
		t.Errorf("LEAK: a pure-semantic row for a page the caller cannot view reached the response: %s", body)
	}
}
