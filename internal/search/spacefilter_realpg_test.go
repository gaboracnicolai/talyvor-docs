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
	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// `space_id` WAS HONOURED BY HALF THE ENDPOINT.
//
// Search takes one `space_id` and runs two queries with it. page.SearchWithRank receives it and
// applies `AND ($3::text IS NULL OR p.space_id = $3)`; SemanticSearch.Search HAS NO SUCH PARAMETER
// — its SQL scopes to `p.workspace_id` and `p.is_template` and nothing else. So "search inside this
// space" returned pages from every OTHER space in the workspace through the semantic path, and
// because a pure-semantic row carries no space_name the answer did not even say which space it
// came from.
//
// ⚠ THIS IS THE FIRST TEST IN THE REPO THAT MAKES POSTGRES COMPILE THE pgvector QUERY. Every other
// semantic test drives pgxmock, which regex-matches the SQL text and never asks a database to plan
// it — the exact blind spot that let SearchWithRank ship SQL that did not compile (#52). That makes
// this a floor as well as a guard: it is the only thing in the tree that would notice the semantic
// SQL becoming invalid.
func TestSearch_SpaceFilter_AppliesToSemanticHalf_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	// TWO PUBLIC SPACES. Public on purpose: the access gate must let both through, so anything
	// missing from the answer is the SPACE FILTER's doing and not the permission engine's.
	spaceA := seedSpace(t, d, ws, alice, "Alpha Space", false)
	spaceB := seedSpace(t, d, ws, alice, "Beta Space", false)
	pageA := seedPage(t, d, ws, spaceA, alice, "Alpha Runbook", "vector body one")
	pageB := seedPage(t, d, ws, spaceB, alice, "Beta Runbook", "vector body two")

	// Identical embeddings, so both pages are equally similar to the query and neither can be
	// dropped by the 0.75 threshold. What separates them in the answer is the space filter alone.
	vec := unitVector()
	for _, id := range []string{pageA, pageB} {
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

	// permitAll isolates this test from the access gate: every page here is in a public space and
	// must come back, so a row that is missing is missing because of the space filter.
	h := NewHandler(page.NewStore(d.Pool), sem).WithAccess(permitAll{})
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(),
				"alice@example.com", []authz.Membership{{WorkspaceID: ws, MemberID: alice}})))
		})
	})
	r.Route("/v1", func(r chi.Router) { h.Mount(r) })

	ask := func(query string) []Result {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/search?"+query, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200: %s", query, rr.Code, rr.Body.String())
		}
		var body response
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, rr.Body.String())
		}
		return body.Results
	}
	ids := func(rs []Result) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.PageID)
		}
		return out
	}
	contains := func(rs []Result, id string) bool {
		return strings.Contains(strings.Join(ids(rs), ","), id)
	}

	// ── PREMISE + POSITIVE CONTROL, in the same sample. With NO space_id the semantic half must
	// return BOTH pages. If it does not, the embeddings or the fake Lens are broken and an absent
	// row below would prove nothing about the filter.
	both := ask("q=vector&type=semantic")
	if !contains(both, pageA) || !contains(both, pageB) {
		t.Fatalf("PREMISE FAILED: unscoped semantic search returned %v, want both %s and %s — "+
			"the instrument cannot see these pages at all", ids(both), pageA, pageB)
	}

	// ── THE DEFECT. Scoped to Alpha, a page in Beta must not come back.
	scoped := ask("q=vector&type=semantic&space_id=" + spaceA)
	if !contains(scoped, pageA) {
		t.Errorf("OVER-CORRECTION: scoping to Alpha lost Alpha's own page (got %v)", ids(scoped))
	}
	if contains(scoped, pageB) {
		t.Errorf("space_id LEAK: search scoped to Alpha (%s) returned Beta's page %s — the semantic "+
			"half ignores space_id (got %v)", spaceA, pageB, ids(scoped))
	}

	// ── AND THE SAME PARAMETER ON type=all, which is the frontend's default, so the merged path is
	// covered rather than only the semantic-only one.
	merged := ask("q=vector&type=all&space_id=" + spaceA)
	if contains(merged, pageB) {
		t.Errorf("space_id LEAK on type=all: scoped to Alpha, got %v including Beta's %s", ids(merged), pageB)
	}
}

// permitAll is the access gate saying yes to everything, so this test measures the space filter
// and only the space filter.
type permitAll struct{}

func (permitAll) AuthorizePageRead(context.Context, string) (bool, bool) { return true, true }

// unitVector renders the 1536-dimension pgvector literal the schema requires (vector(1536)). The
// same vector is used for both pages AND for the query, so every cosine similarity is 1.0 and the
// 0.75 threshold cannot be what removes a row.
func unitVector() string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 1536; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if i == 0 {
			b.WriteString("1")
		} else {
			b.WriteString("0")
		}
	}
	b.WriteByte(']')
	return b.String()
}
