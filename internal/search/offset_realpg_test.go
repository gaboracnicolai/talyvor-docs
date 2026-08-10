package search

import (
	"context"
	"encoding/json"
	"fmt"
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

// `offset` WAS HONOURED BY HALF THE ENDPOINT — THE OTHER HALF OF #75's DEFECT, ON THE OTHER
// PAGINATION PARAMETER.
//
// The handler reads one `offset` and runs two queries. page.SearchWithRank receives it and applies
// `LIMIT $4 OFFSET $5`; SemanticSearch.Search HAD NO SUCH PARAMETER — its SQL was `LIMIT $3` and
// nothing more. Measured through this endpoint before the fix, four pages, distinct similarities:
//
//	type=semantic limit=2 offset=0  ->  one two
//	type=semantic limit=2 offset=2  ->  one two      ← page 2 IS page 1
//	type=semantic limit=2 offset=4  ->  one two      ← past the end, and still page 1
//	type=all      limit=2 offset=0  ->  one two      (source "both")
//	type=all      limit=2 offset=2  ->  one two      (source "semantic")
//	type=fulltext limit=2 offset=2  ->  three four   ← the half that was already right
//
// ⚠ IT IS NOT ONLY A REPEAT. `offset=4` on four rows returned page 1 rather than nothing, so the
// semantic tail was UNREACHABLE at every offset: no value of `offset` could ever show page three or
// four through the semantic path.
//
// ⚠ AND ON type=all — THE FRONTEND'S DEFAULT — THE STALE ROWS DISPLACED THE CORRECT ONES. The
// full-text half paged correctly to three/four; the merge then re-ranked, the stale semantic rows
// scored higher (similarity 0.9988×0.4 against a ts_rank×0.6 of ~0.04), and the truncation to
// `limit` dropped three and four entirely. Page 2 of the default search showed page 1's documents
// and hid the ones the working half had just fetched. The `source` flag flipped from "both" to
// "semantic" for the same row across the two pages, for the same reason.
//
// ⚠ THE SIMILARITIES HERE ARE DISTINCT ON PURPOSE, so `ORDER BY pe.embedding <=> $1` is a TOTAL
// order and this test measures the offset alone. Whether that ORDER BY is safe to page over when
// similarities TIE is a separate, measured, unfixed question — see the note on Search in
// semantic.go and the queue item.
func TestSearch_Offset_AppliesToSemanticHalf_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	// ONE PUBLIC SPACE. Public on purpose: the access gate must let every page through, so a row
	// missing from an answer is the OFFSET's doing and not the permission engine's.
	sp := seedSpace(t, d, ws, alice, "Alpha Space", false)

	// Four pages, ordered the SAME WAY by both halves so a page boundary means one thing:
	//   · semantic — embedding [1, eps, 0…] with eps ascending ⇒ cosine similarity descending,
	//     all four far above the 0.75 threshold, no two equal.
	//   · full-text — the query term repeated a descending number of times ⇒ ts_rank descending.
	// Without that, a tie in either ORDER BY would make "page 2" a statement about the planner.
	names := []string{"one", "two", "three", "four"}
	ids := make([]string, 0, len(names))
	label := map[string]string{}
	for i, n := range names {
		body := strings.TrimSpace(strings.Repeat("vector ", len(names)-i)) + " body " + n
		id := seedPage(t, d, ws, sp, alice, "Runbook "+n, body)
		ids = append(ids, id)
		label[id] = n
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO page_embeddings (page_id, embedding) VALUES ($1, $2::vector)`,
			id, vecAt(0.05*float64(i+1))); err != nil {
			t.Fatalf("seed embedding for %s: %v", n, err)
		}
	}

	lens := newFakeLens(t)
	defer lens.Close()
	query := vecAt(0)
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + query + `}]}`))
	}
	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	// permitAll isolates this from the access gate — every page is in a public space — AND it is
	// the state that over-fetches (fetchLimit = 4×limit), which is the harder half: the semantic
	// LIMIT is then wider than the page, so a missing OFFSET cannot be masked by a narrow window.
	h := NewHandler(page.NewStore(d.Pool), sem).WithAccess(permitAll{})
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authz.WithMemberships(req.Context(),
				"alice@example.com", []authz.Membership{{WorkspaceID: ws, MemberID: alice}})))
		})
	})
	r.Route("/v1", func(r chi.Router) { h.Mount(r) })

	ask := func(query string) []string {
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
		out := make([]string, 0, len(body.Results))
		for _, res := range body.Results {
			out = append(out, label[res.PageID])
		}
		return out
	}
	eq := func(got []string, want ...string) bool {
		return strings.Join(got, ",") == strings.Join(want, ",")
	}

	// ── PREMISE + POSITIVE CONTROL, in the same sample. Unpaged, the semantic half must return all
	// four in similarity order. If it does not, the embeddings, the fake Lens or the seeding are
	// broken and every absence below would prove nothing about the offset.
	if all := ask("q=vector&type=semantic&limit=4&offset=0"); !eq(all, "one", "two", "three", "four") {
		t.Fatalf("PREMISE FAILED: unpaged semantic search returned %v, want [one two three four] — "+
			"the instrument cannot see these four pages in a known order at all", all)
	}
	// The same premise for the full-text half, which is what makes the type=all cases below a
	// statement about the merge rather than about ts_rank.
	if all := ask("q=vector&type=fulltext&limit=4&offset=0"); !eq(all, "one", "two", "three", "four") {
		t.Fatalf("PREMISE FAILED: unpaged full-text search returned %v, want [one two three four]", all)
	}

	// ── OVER-CORRECTION REFUSAL. Page 1 must be page 1: an offset applied where none was asked for
	// is the mirror-image defect, and nothing else in this file would notice it.
	if got := ask("q=vector&type=semantic&limit=2&offset=0"); !eq(got, "one", "two") {
		t.Errorf("OVER-CORRECTION: semantic page 1 (offset=0) = %v, want [one two]", got)
	}

	// ── THE DEFECT. Page 2 of the semantic half must be the SECOND pair, not the first again.
	if got := ask("q=vector&type=semantic&limit=2&offset=2"); !eq(got, "three", "four") {
		t.Errorf("offset IGNORED by the semantic half: page 2 (limit=2 offset=2) = %v, want "+
			"[three four] — the semantic SQL takes no OFFSET, so it re-serves page 1", got)
	}

	// ── AND THE TAIL IS REACHABLE. Past the last row the answer is empty; before the fix this
	// returned page 1, which is why no offset could ever show pages three and four.
	if got := ask("q=vector&type=semantic&limit=2&offset=4"); len(got) != 0 {
		t.Errorf("offset PAST THE END returned %v, want [] — with the offset dropped, every page "+
			"of this search is page 1 and the tail is unreachable", got)
	}

	// ── type=all IS THE FRONTEND'S DEFAULT, so the merged path is covered rather than only the
	// semantic-only one — and it is where the stale rows DISPLACE the correctly-paged full-text
	// ones: three and four were fetched by the working half and dropped by the truncation.
	if got := ask("q=vector&type=all&limit=2&offset=0"); !eq(got, "one", "two") {
		t.Errorf("OVER-CORRECTION on type=all: page 1 = %v, want [one two]", got)
	}
	if got := ask("q=vector&type=all&limit=2&offset=2"); !eq(got, "three", "four") {
		t.Errorf("offset IGNORED on type=all: page 2 = %v, want [three four] — the full-text half "+
			"paged correctly and the stale semantic rows out-ranked and replaced its answer", got)
	}

	// ── MUST STAY GREEN: the half that was already right. A fix that threads the offset through
	// the semantic side and breaks the full-text side would be caught here and nowhere else in
	// this file, because every other case above can be satisfied by the semantic rows alone.
	if got := ask("q=vector&type=fulltext&limit=2&offset=2"); !eq(got, "three", "four") {
		t.Errorf("REGRESSION on the full-text half: page 2 = %v, want [three four]", got)
	}
}

// vecAt renders [1, eps, 0…] as the 1536-dimension pgvector literal the schema requires
// (vector(1536)). Cosine similarity to vecAt(0) is 1/sqrt(1+eps²) — strictly decreasing in eps and
// never below 0.98 for the values used here, so the 0.75 threshold cannot be what removes a row and
// no two rows can tie.
func vecAt(eps float64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 1536; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		switch i {
		case 0:
			b.WriteString("1")
		case 1:
			fmt.Fprintf(&b, "%g", eps)
		default:
			b.WriteString("0")
		}
	}
	b.WriteByte(']')
	return b.String()
}
