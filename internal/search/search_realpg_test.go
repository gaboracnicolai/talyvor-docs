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
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// THE ENDPOINT, OVER A REAL POSTGRES AND A REAL STORE.
//
// Every other test in this package injects a fakePages that returns a hand-built slice, so the
// whole package was green while the endpoint returned 500 to every caller. Measured at `34ab2d5`
// with a matching page seeded in the workspace:
//
//	GET /v1/workspaces/{ws}/search?q=runbook  ->  HTTP 500 {"error":"search failed"}
//
// because page.SearchWithRank's SQL did not compile (see the sibling guard in internal/page).
// This test is the consumer-side floor: the fake cannot fail the way production failed, so the
// endpoint needs one test that talks to a database.
func TestSearch_RealPG_ReturnsTheMatchingPage(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	author := d.Member(t, ws, "alice@example.com")
	pageID := d.Page(t, ws, author, "Deployment Runbook")

	if _, err := d.Pool.Exec(ctx,
		`UPDATE pages SET content_text = 'the deployment runbook for the auth flow' WHERE id = $1`,
		pageID); err != nil {
		t.Fatalf("seed content_text: %v", err)
	}

	// A real store, and a semantic side with no Lens configured — Search returns [] there, which
	// is the graceful-degradation contract, so the full-text half is what this exercises.
	s := page.NewStore(d.Pool)
	sem := newSemanticSearch(lensintegration.New("", ""), nil)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authz.WithMemberships(req.Context(), "alice@example.com",
				[]authz.Membership{{WorkspaceID: ws, MemberID: author}})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/v1", func(r chi.Router) { NewHandler(s, sem).Mount(r) })

	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/search?q=runbook", nil)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /search = HTTP %d %s\nThe product's only search endpoint is down for every "+
			"caller with a real database behind it.", rr.Code, rr.Body.String())
	}
	var resp response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rr.Body.String())
	}
	if len(resp.Results) != 1 {
		t.Fatalf("want 1 result for a workspace with one matching page, got %d (body %s)",
			len(resp.Results), rr.Body.String())
	}
	got := resp.Results[0]
	if got.PageID != pageID {
		t.Fatalf("result page_id = %q, want %q", got.PageID, pageID)
	}
	if got.SpaceName == "" {
		t.Fatal("space_name is empty — the JOIN column did not survive the projection")
	}
	if got.Headline == "" {
		t.Fatal("headline is empty — ts_headline did not survive the projection")
	}
}
