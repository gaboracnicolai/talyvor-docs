package search

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/testutil"
)

// THE SEMANTIC HALF HAS THE IDENTICAL DEFECT, AND ITS TIES ARE EASIER TO COME BY THAN THE FULL-TEXT
// HALF'S — TWO PAGES WITH THE SAME TEXT EMBED TO THE SAME VECTOR, SO THEY SIT AT THE SAME DISTANCE.
//
// `SemanticSearch.Search` ends `ORDER BY pe.embedding <=> $1::vector`. A distance is not a key.
// Rows at an equal distance have no defined relative order, and `LIMIT $3 OFFSET $5` over an
// undefined order repeats rows onto later pages and drops others entirely — see the sibling guard
// in internal/page for the measurement of the mechanism (a bounded sort is a top-N heapsort whose
// N is limit+offset, so every page runs a different sort).
//
// ⚠ THIS HALF ALSO FLIPS ON THE PLANNER, AND THAT WAS MEASURED SEPARATELY BEFORE THE FIX. Six
// pages with identical embeddings, the same query, the ONLY difference the plan:
//
//	Seq Scan on pages     ->  [one two three four five six]
//	Index Scan pages_pkey ->  [one four two three five six]
//
// Compose page 1 from the first with page 2 from the second — which is what happens when the
// planner's choice changes between two requests — and `three` is served twice while `four` is
// never served. A total order removes both exposures at once, so the shipped guard asserts the
// product property (paging shows each row exactly once) rather than pinning a plan.
func TestSemanticSearch_PagingATiedResultSetLosesRows_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")
	sp := seedSpace(t, d, ws, alice, "Runbooks", false)

	const (
		total = 40
		limit = 5
	)
	seeded := map[string]int{}
	for i := 0; i < total; i++ {
		id := seedPage(t, d, ws, sp, alice, fmt.Sprintf("Runbook %02d", i), "the deployment runbook")
		seeded[id] = i
		// IDENTICAL embeddings — the same distance from the query for every row. vecAt(0.05) is far
		// above the 0.75 similarity threshold, so nothing is filtered out by relevance.
		if _, err := d.Pool.Exec(ctx,
			`INSERT INTO page_embeddings (page_id, embedding) VALUES ($1, $2::vector)`,
			id, vecAt(0.05)); err != nil {
			t.Fatalf("seed embedding %d: %v", i, err)
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

	// PRECONDITION, ASSERTED RATHER THAN ASSUMED: all 40 rows clear the threshold and all 40
	// similarities are equal. A green below must mean "the order is total", never "the set was
	// empty" or "the distances were distinct anyway".
	all, err := sem.Search(ctx, ws, "runbook", nil, 50, 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != total {
		t.Fatalf("precondition: an unpaged semantic search returned %d rows, want %d — the fixture "+
			"is not the tied set this test is about", len(all), total)
	}
	for _, r := range all[1:] {
		if r.Similarity != all[0].Similarity {
			t.Fatalf("precondition: similarities are NOT tied (%v vs %v) — this fixture cannot "+
				"exercise the tie-order defect at all", r.Similarity, all[0].Similarity)
		}
	}

	seen := map[string]int{}
	var order []int
	for off := 0; off < total; off += limit {
		got, err := sem.Search(ctx, ws, "runbook", nil, limit, off)
		if err != nil {
			t.Fatalf("Search(offset=%d): %v", off, err)
		}
		if len(got) != limit {
			t.Errorf("semantic page at offset %d returned %d rows, want %d", off, len(got), limit)
		}
		for _, r := range got {
			seen[r.PageID]++
			order = append(order, seeded[r.PageID])
		}
	}

	var repeated, missing []int
	for id, n := range seen {
		if n > 1 {
			repeated = append(repeated, seeded[id])
		}
	}
	for id, i := range seeded {
		if seen[id] == 0 {
			missing = append(missing, i)
		}
	}
	sort.Ints(repeated)
	sort.Ints(missing)

	if len(repeated) > 0 || len(missing) > 0 {
		t.Errorf("paging %d equidistant semantic hits %d at a time showed some pages twice and "+
			"others never:\n"+
			"  repeated: %v\n"+
			"  never shown: %v\n"+
			"  order served: %v\n"+
			"`ORDER BY pe.embedding <=> $1::vector` names no unique column, so equidistant rows "+
			"have no defined relative order. Paging is only well defined over a TOTAL order — add "+
			"a unique tiebreaker (pe.page_id).",
			total, limit, repeated, missing, order)
	}
}
