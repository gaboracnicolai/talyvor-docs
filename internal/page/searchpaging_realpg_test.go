package page_test

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// PAGING A TIED SEARCH REPEATS ONE ROW ON EVERY PAGE AND NEVER SHOWS ANOTHER — AND IT TAKES NO
// CONCURRENCY, NO SECOND CONNECTION AND NO PLANNER FLIP TO DO IT.
//
// `SearchWithRank` ends `ORDER BY rank DESC`. `rank` is a ts_rank score, not a key: any two pages
// whose title+body produce the same tsvector against the same query score IDENTICALLY, and a
// workspace that templated its runbooks has dozens. An ORDER BY that names no unique column leaves
// tied rows in NO defined relative order, so `LIMIT n OFFSET k` is asking a question the query does
// not answer — and Postgres is free to answer it differently for every k.
//
// ⚠ IT DOES. Measured on pgvector/pgvector:pg16 against 40 tied rows, one connection, one plan
// (Seq Scan → Sort → Limit), nothing written between the requests:
//
//	page 1 (limit 5 offset  0)  ->  {2 3 4 5 1}
//	page 2 (limit 5 offset  5)  ->  {7 8 9 10 1}
//	page 3 (limit 5 offset 10)  ->  {12 13 14 15 1}
//
// Row 1 is on EVERY page. Rows 6, 11 and 16 are on none of them. The cause is not a race and not
// the planner: a bounded sort is a TOP-N HEAPSORT whose N is `limit + offset`, so each page runs a
// DIFFERENT sort, and a different N permutes an all-equal input differently. Paging is the thing
// that changes N, so paging is the thing that breaks it.
//
// ⚠ THIS IS WHY THE GUARD ASSERTS ABOUT THE PRODUCT AND NEVER ABOUT THE PLAN. The queue recorded
// this exposure as needing "a real-PG test that pages with `SET enable_seqscan` flipped BETWEEN the
// two requests … a guard that asserts about the planner rather than about the product". Measured,
// that is not what it needs: four plans (seq scan, bitmap heap scan, two index scans) all agree on
// this data, and the defect reproduces under a single one of them. The planner was never the
// mechanism — it was one more thing that could also change the answer.
//
// ⚠ WHAT THIS TEST CANNOT SEE, said so the next session does not read more into a green: it asserts
// that ONE plan pages honestly. Two requests served by two DIFFERENT plans is a second exposure with
// the same cause and the same fix, and the fix closes both by construction — a total order is a
// total order under every plan — but only the single-plan half is measured here.
func TestSearchWithRank_PagingATiedResultSetLosesRows_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@example.com")

	var spaceID string
	if err := d.Pool.QueryRow(ctx,
		`INSERT INTO spaces (workspace_id, name, slug, created_by)
		 VALUES ($1, 'Runbooks', 'runbooks', $2) RETURNING id`,
		ws, alice).Scan(&spaceID); err != nil {
		t.Fatalf("seed space: %v", err)
	}

	// 40 pages with IDENTICAL searchable text. The tie is the point: templated runbooks are the
	// ordinary way a workspace gets one, and the ranking function cannot tell them apart.
	const (
		total = 40
		limit = 5
	)
	seeded := map[string]int{}
	for i := 0; i < total; i++ {
		var id string
		if err := d.Pool.QueryRow(ctx,
			`INSERT INTO pages (space_id, workspace_id, title, slug, created_by, content_text)
			 VALUES ($1, $2, 'Runbook', $3, $4, 'the deployment runbook for the auth flow')
			 RETURNING id`,
			spaceID, ws, fmt.Sprintf("runbook-%02d", i), alice).Scan(&id); err != nil {
			t.Fatalf("seed page %d: %v", i, err)
		}
		seeded[id] = i
	}

	s := page.NewStore(d.Pool)

	// PRECONDITION, ASSERTED RATHER THAN ASSUMED: every seeded page matches the query and the ranks
	// really do tie. Without this a green below could mean "the ORDER BY is total" or "there was
	// nothing to order" — and those are not the same result.
	all, err := s.SearchWithRank(ctx, ws, "runbook", nil, 50, 0)
	if err != nil {
		t.Fatalf("SearchWithRank: %v", err)
	}
	if len(all) != total {
		t.Fatalf("precondition: an unpaged search returned %d rows, want %d — the fixture is not "+
			"the tied set this test is about", len(all), total)
	}
	for _, r := range all[1:] {
		if r.Rank != all[0].Rank {
			t.Fatalf("precondition: ranks are NOT tied (%v vs %v) — this fixture cannot exercise "+
				"the tie-order defect at all", r.Rank, all[0].Rank)
		}
	}

	// Page through the whole set the way a client does: same query, same limit, offset advancing.
	seen := map[string]int{}
	var order []int
	for off := 0; off < total; off += limit {
		got, err := s.SearchWithRank(ctx, ws, "runbook", nil, limit, off)
		if err != nil {
			t.Fatalf("SearchWithRank(offset=%d): %v", off, err)
		}
		if len(got) != limit {
			t.Errorf("page at offset %d returned %d rows, want %d", off, len(got), limit)
		}
		for _, r := range got {
			seen[r.Page.ID]++
			order = append(order, seeded[r.Page.ID])
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
		t.Errorf("paging %d tied rows %d at a time showed some pages twice and others never:\n"+
			"  repeated: %v\n"+
			"  never shown: %v\n"+
			"  order served: %v\n"+
			"`ORDER BY rank DESC` names no unique column, so tied rows have no defined relative "+
			"order and each page runs a differently-bounded sort. Paging is only well defined over "+
			"a TOTAL order — add a unique tiebreaker (p.id).",
			total, limit, repeated, missing, order)
	}
}
