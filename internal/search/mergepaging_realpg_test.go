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

// PAGING A MERGED SEARCH STRANDED THE ROWS THE MERGE HAD NOT SHOWN YET — AT EVERY OFFSET.
//
// #75 threaded `offset` into the semantic half's SQL so both halves page. That closed the half
// that repeated page 1. THIS is the half that is left: the handler runs TWO ordered queries and
// then RE-RANKS their rows into ONE answer, so row k of a half is not row k of the answer. An
// offset pushed into each half's `LIMIT/OFFSET` therefore skips rows by their position IN THEIR
// OWN HALF, and the merge had not yet shown them.
//
// MEASURED ON REAL POSTGRES THROUGH THE SHIPPED ROUTE, six documents in one public space (the
// access gate says yes to everything, so nothing here is the permission engine's doing):
//
//	full-text only, "vector" repeated 4/3/2/1 times   ->  A B C D   (ts_rank descending)
//	semantic only, no query term, embeddings near it  ->  X Y       (cosine descending)
//
//	GET ?type=all&limit=10&offset=0   ->  X Y A B C D      the whole corpus, correctly ranked
//	GET ?type=all&limit=2&offset=0    ->  X Y
//	GET ?type=all&limit=2&offset=2    ->  C D              ← A and B skipped
//	GET ?type=all&limit=2&offset=4    ->  []               ← the walk ENDS
//	GET ?type=all&limit=2&offset=6    ->  []
//
// A and B are THE TWO STRONGEST FULL-TEXT MATCHES FOR THE QUERY and they appear on NO page. The
// walk does not merely reorder them — it terminates cleanly having never shown them, so a
// consumer that pages to exhaustion has no signal that anything is missing.
//
// ⚠ WHY THE SCORES MAKE THIS THE ORDINARY CASE RATHER THAN A CONTRIVED ONE. merge() scores a row
// as max(ts_rank×0.6, similarity×0.4). ts_rank on a short document is ~0.05–0.1, so ×0.6 is
// ~0.03–0.06; cosine similarity must clear 0.75 to be returned at all, so ×0.4 is ≥0.3. Any
// semantic hit therefore outranks any full-text hit, and the merged order differs from the
// full-text order whenever the semantic half returns anything. type=all is the frontend's
// default and the API's default.
//
// ⚠ THE RULE THE FIX APPLIES, STATED SO IT IS NOT READ AS A SPECIAL CASE. The SQL offset is
// correct exactly when there is ONE ordered source: with type=fulltext or type=semantic the
// half's ORDER BY *is* the answer's order, so LIMIT/OFFSET names the caller's page. With
// type=all there are TWO, and only the merged ranking knows where a page begins — so the window
// is fetched from row 0 and the offset is applied AFTER the merge and AFTER the access filter.
// [SINGLE-HALF] holds the one-source paths to the SQL offset, which is what keeps #75's fix
// reachable in production and keeps deep single-type paging working.
//
// ⚠ WHAT THIS DOES NOT DO, STATED RATHER THAN IMPLIED. On type=all the merged pool is whatever
// the two halves' own clamps allow (each clamps limit to 50), so a page whose offset lies past
// that pool is SHORT or EMPTY rather than wrong. That is the same "a filtered page is a short
// page" property the access gate already has, and it is a different question from this one:
// reaching further needs a cursor, not a bigger number.
func TestSearch_MergedPaging_DoesNotStrandRows_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	// ONE PUBLIC SPACE, so a row missing from an answer is the paging's doing and not the
	// permission engine's.
	sp := seedSpace(t, d, ws, alice, "Alpha Space", false)

	label := map[string]string{}

	// FULL-TEXT ONLY — they contain the query term and have NO embedding, so they exist in
	// exactly one half. The term is repeated a descending number of times, so ts_rank is a
	// total order and "page 2" is not a statement about the planner.
	ftNames := []string{"A", "B", "C", "D"}
	for i, n := range ftNames {
		body := strings.TrimSpace(strings.Repeat("vector ", len(ftNames)-i)) + " runbook " + n
		label[seedPage(t, d, ws, sp, alice, "FT "+n, body)] = n
	}
	// SEMANTIC ONLY — no occurrence of the query term anywhere in title or body, so the
	// full-text half cannot return them, and an embedding near the query vector so the semantic
	// half must. Distinct epsilons keep cosine similarity a total order; both are ≥0.99, far
	// above the 0.75 threshold, so relevance filtering cannot be what removes a row.
	semNames := []string{"X", "Y"}
	for i, n := range semNames {
		id := seedPage(t, d, ws, sp, alice, "SEM "+n, "unrelated prose about kittens "+n)
		label[id] = n
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO page_embeddings (page_id, embedding) VALUES ($1, $2::vector)`,
			id, vecAt(0.05*float64(i+1))); err != nil {
			t.Fatalf("seed embedding for %s: %v", n, err)
		}
	}

	lens := newFakeLens(t)
	defer lens.Close()
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vecAt(0) + `}]}`))
	}
	sem := newSemanticSearch(lensintegration.New(lens.URL, "k1"), d.Pool).
		WithLensURL(lens.URL).
		WithTokenProvider(lenscreds.New(lens.URL, "k1", lenscreds.Options{}))

	// permitAll isolates this from the access gate AND is the state that over-fetches
	// (fetchLimit = maxFetchFactor × window), which is the harder half: a narrow window could
	// otherwise mask what the offset does.
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
			t.Fatalf("decode %s: %v (%s)", query, err, rr.Body.String())
		}
		out := make([]string, 0, len(body.Results))
		for _, res := range body.Results {
			name, ok := label[res.PageID]
			if !ok {
				t.Fatalf("%s returned an unseeded page %s", query, res.PageID)
			}
			out = append(out, name)
		}
		return out
	}
	joined := func(v []string) string { return strings.Join(v, ",") }

	// ── PREMISE + POSITIVE CONTROL IN ONE SAMPLE. Unpaged, the endpoint must see all six in a
	// known order. If it does not, the seeding, the fake Lens or the embeddings are broken and
	// every absence below would prove nothing about paging.
	const wantAll = "X,Y,A,B,C,D"
	unpaged := ask("q=vector&type=all&limit=10&offset=0")
	if joined(unpaged) != wantAll {
		t.Fatalf("[PREMISE-UNPAGED] unpaged type=all returned %v, want [%s] — the instrument "+
			"cannot see these six documents in a known order at all, so nothing below is a "+
			"measurement of the offset", unpaged, wantAll)
	}
	// The two halves in isolation, so the merged expectations above are a statement about the
	// MERGE and not about either query.
	if got := ask("q=vector&type=fulltext&limit=10&offset=0"); joined(got) != "A,B,C,D" {
		t.Fatalf("[PREMISE-UNPAGED] unpaged type=fulltext returned %v, want [A B C D]", got)
	}
	if got := ask("q=vector&type=semantic&limit=10&offset=0"); joined(got) != "X,Y" {
		t.Fatalf("[PREMISE-UNPAGED] unpaged type=semantic returned %v, want [X Y]", got)
	}

	// ── THE DEFECT. Walk type=all two rows at a time until the endpoint says there is no more,
	// and compare what the walk SAW with what one unpaged request returns.
	//
	// The walk is bounded by an iteration cap rather than by the corpus size so that a fix that
	// never terminates is a failure here rather than a hung suite.
	var walk []string
	seenOn := map[string]int{}
	dupes := []string{}
	for off, page := 0, 0; page < 12; off, page = off+2, page+1 {
		got := ask(fmt.Sprintf("q=vector&type=all&limit=2&offset=%d", off))
		if len(got) == 0 {
			break
		}
		for _, n := range got {
			if prev, dup := seenOn[n]; dup {
				dupes = append(dupes, fmt.Sprintf("%s (offset %d and %d)", n, prev, off))
			}
			seenOn[n] = off
		}
		walk = append(walk, got...)
	}

	// [PAGE-ORDER] is the strong form: a paged walk must reproduce the unpaged answer exactly,
	// in order. It subsumes [UNREACHABLE] and is asserted first so a fix that returns every row
	// in a SHUFFLED order is not scored as correct.
	if joined(walk) != wantAll {
		t.Errorf("[PAGE-ORDER] walking type=all at limit=2 saw %v; one unpaged request returns "+
			"[%s]. The offset is pushed into BOTH halves' SQL while merge() re-ranks across "+
			"them, so each page skips rows by their position in their own half rather than in "+
			"the answer.", walk, wantAll)
	}

	// [UNREACHABLE] names the documents the walk never showed. Separate from [PAGE-ORDER]
	// because "a row is on no page at all" and "the pages are in the wrong order" are different
	// failures with different repairs, and only the first one loses data.
	var stranded []string
	for _, n := range append(append([]string{}, ftNames...), semNames...) {
		if _, ok := seenOn[n]; !ok {
			stranded = append(stranded, n)
		}
	}
	if len(stranded) > 0 {
		t.Errorf("[UNREACHABLE] %v appear on NO page of a type=all walk, while one unpaged "+
			"request returns all six. The walk terminated with an empty page, so a consumer "+
			"paging to exhaustion receives no signal that anything is missing.", stranded)
	}

	// [NO-DUPES] refuses the mirror-image repair. A fix that stops stranding rows by serving an
	// overlapping window would satisfy both assertions above and still be wrong.
	if len(dupes) > 0 {
		t.Errorf("[NO-DUPES] a document was served on two different pages of one walk: %v", dupes)
	}

	// ── MUST STAY GREEN: THE ONE-SOURCE PATHS STILL PAGE THROUGH THE SQL OFFSET. With a single
	// half running there is nothing to re-rank across, so the half's own LIMIT/OFFSET names the
	// caller's page — and it is the only thing that can reach past the merged pool. An
	// over-broad fix that moves EVERY offset behind the merge breaks deep single-type paging,
	// and nothing else in this file would notice.
	for _, c := range []struct{ query, want string }{
		{"q=vector&type=fulltext&limit=2&offset=0", "A,B"},
		{"q=vector&type=fulltext&limit=2&offset=2", "C,D"},
		{"q=vector&type=fulltext&limit=2&offset=4", ""},
		{"q=vector&type=semantic&limit=1&offset=0", "X"},
		{"q=vector&type=semantic&limit=1&offset=1", "Y"},
		{"q=vector&type=semantic&limit=1&offset=2", ""},
	} {
		if got := ask(c.query); joined(got) != c.want {
			t.Errorf("[SINGLE-HALF] %s = %v, want [%s] — one ordered source pages through its "+
				"own SQL offset", c.query, got, c.want)
		}
	}

	// ── OVER-CORRECTION REFUSAL. Page 1 must be page 1. An offset applied where none was asked
	// for is the mirror-image defect and every assertion above can be satisfied without it.
	if got := ask("q=vector&type=all&limit=2&offset=0"); joined(got) != "X,Y" {
		t.Errorf("[OVER-CORRECTION] type=all page 1 (offset=0) = %v, want [X Y]", got)
	}
	// And a page past the end is empty rather than a wrapped-around repeat.
	if got := ask("q=vector&type=all&limit=2&offset=6"); len(got) != 0 {
		t.Errorf("[OVER-CORRECTION] type=all offset=6 over six rows = %v, want []", got)
	}
}

// SINGLE-SOURCE PAGING MUST STILL REACH PAST THE MERGED WINDOW.
//
// ⚠ THIS GUARD IS NOT RED-FIRST AND SAYING SO IS THE POINT: it is GREEN BEFORE AND AFTER the fix
// above, because it exists to bound that fix's BLAST RADIUS rather than to catch the defect. The
// obvious over-broad repair — apply the offset after the merge for EVERY `type` — passes every
// assertion in the test above (its corpus is six rows, so no window can truncate it) and silently
// caps single-type paging at the merged window. Nothing else in this package would notice.
//
// The numbers: both halves clamp `limit` to 50, so a type=all window is bounded and a page past it
// is empty by construction. A ONE-SOURCE search has no such bound — its SQL `OFFSET` reaches as
// far as the caller asks — and MCP's `search_docs` and any API consumer paging a corpus larger
// than 50 depend on that. 60 documents and offset=52 is the smallest fixture that can tell the two
// implementations apart.
func TestSearch_SingleSourcePagingReachesPastTheMergedWindow_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	sp := seedSpace(t, d, ws, alice, "Deep Space", false)

	// 60 full-text-only documents — no embeddings at all, so the semantic half returns nothing
	// and this is unambiguously the one-source path. Rank order is deliberately NOT asserted
	// (60 repetition counts do not give 60 distinct ts_ranks); what is asserted is that a page
	// past the merged window is SERVED, which is the property the over-broad repair destroys.
	const corpus = 60
	for i := 0; i < corpus; i++ {
		seedPage(t, d, ws, sp, alice, fmt.Sprintf("Deep %02d", i), "vector runbook deep")
	}

	lens := newFakeLens(t)
	defer lens.Close()
	lens.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":` + vecAt(0) + `}]}`))
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

	count := func(query string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/"+ws+"/search?"+query, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200: %s", query, rr.Code, rr.Body.String())
		}
		var body response
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		return len(body.Results)
	}

	// PREMISE: the corpus really is bigger than the window, measured through the route rather
	// than assumed from the seed loop. Without this the assertion below could pass on an empty
	// corpus for the wrong reason.
	if n := count("q=vector&type=fulltext&limit=50&offset=0"); n != maxFetchRows {
		t.Fatalf("[PREMISE-DEEP] a limit=50 page returned %d rows, want %d — the fixture is not "+
			"larger than the merged window, so nothing below discriminates anything", n, maxFetchRows)
	}
	if n := count("q=vector&type=fulltext&limit=2&offset=52"); n != 2 {
		t.Errorf("[SINGLE-HALF-DEEP] type=fulltext limit=2 offset=52 over %d documents returned "+
			"%d rows, want 2. One ordered source pages through its own SQL OFFSET, which is not "+
			"bounded by the merged window — moving that offset behind the merge caps every "+
			"single-type search at %d rows.", corpus, n, maxFetchRows)
	}
	// ⚠ ITS OWN TAG, AND THE CONTROL RUN IS WHY. This started as a second [SINGLE-HALF-DEEP]
	// assertion and it is not the same claim: that tag says "one source reaches past the merged
	// window", this one says "two sources do NOT". Sharing a tag made C1 (the fix reverted
	// wholesale) and C3 (no post-merge offset) both fire [SINGLE-HALF-DEEP], which reads as the
	// blast-radius guard catching the original defect — it does not, and a tag that means two
	// things cannot isolate either.
	//
	// The claim: type=all is bounded by the merged pool BY CONSTRUCTION, so a page past it is
	// empty rather than a wrong continuation. Before the fix this returned 2 rows — the full-text
	// half's rows 52-53, which are not row 52 of the merged answer and were never on any page of
	// it. The semantic half of this fixture is empty, so nothing here is the threshold's doing.
	if n := count("q=vector&type=all&limit=2&offset=52"); n != 0 {
		t.Errorf("[MERGED-WINDOW] type=all limit=2 offset=52 returned %d rows, want 0 — the "+
			"two-source path is bounded by the merged pool, and rows past it are not a "+
			"continuation of the merged ranking", n)
	}
}
