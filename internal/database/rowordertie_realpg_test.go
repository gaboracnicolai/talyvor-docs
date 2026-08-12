package database_test

// TIED `position` VALUES ARE REACHABLE THROUGH THE SHIPPED ROUTES, AND THE READ THAT SERVES THEM
// NAMED NO UNIQUE COLUMN — so a row the user just added came back ABOVE a row that was already
// there.
//
// `page/store.go` writes the rule this file enforces down in its own words: an ORDER BY that names
// no unique column has NO defined relative order. `ListRows` was `ORDER BY position ASC` alone —
// the one list read of user-ordered content in this repo with no tiebreak of any kind — and
// `sortRows` is a `sort.SliceStable`, so whatever undefined order Postgres handed back was
// faithfully preserved into the response.
//
// ⚠ THE TIE IS NOT EXOTIC, IT IS THE ORDINARY SEQUENCE. `DatabaseBlock.tsx:89` posts
// `position: rows.length + 1`, so:
//
//	"+ New" x3            -> alpha@1, bravo@2, charlie@3
//	delete the middle     -> alpha@1, charlie@3          (2 rows)
//	"+ New"               -> delta@3                     ← rows.length + 1 = 3, TIED WITH charlie
//
// ⚠ AND THE ANSWER THEN DEPENDS ON THE HEAP, NOT ON THE DATA. MEASURED on real Postgres, same
// three rows, same SQL:
//
//	before the space is reclaimed   alpha, charlie, delta      (creation order, by luck)
//	after it is                     alpha, DELTA, charlie      ← delta reused bravo's slot
//
// The reclaim is not something this test invents: autovacuum does it continuously in production.
// `VACUUM` below only makes the moment deterministic. A second, independent manifestation was
// measured at the SQL level and is NOT asserted here because it needs planner GUCs: with heap
// order and index order diverged, a Seq Scan answered `A,C,B` and a Bitmap Heap Scan answered
// `A,B,C` for one unchanged table — the plan, chosen from table size, deciding what the user sees.
//
// WHAT THE TIEBREAK IS AND WHY: `position ASC, created_at ASC, id ASC`. `created_at` makes ties
// read as creation order, which is the only reading a user can predict and what the un-reclaimed
// heap was accidentally giving; `id` (the PRIMARY KEY) is what makes the order TOTAL, because
// `created_at` is `NOW()` — transaction time — and two rows written in one transaction share it.
// `block/store.go` already sorts sibling blocks `position ASC, created_at ASC`; it is the same
// shape one column short of total, and this file's `[TIE-ORDER-IS-CREATION-ORDER]` is the
// assertion it does not have.
//
// ⚠ WHAT THIS FILE IS BLIND TO, MEASURED NOT GUESSED (controls in
// ~/talyvor-queue/w31-rowordertie-controls-5e91.py, 7/8 as predicted):
//
//   - E2 — dropping `id ASC` and keeping `created_at ASC` is NOT CAUGHT. Every row here is created
//     in its own HTTP request, so no two share `created_at`. `id` is carried for the
//     single-transaction case, which no shipped route reaches today. Documented-inert.
//   - E7 — `sort.SliceStable` swapped for `sort.Slice` is NOT CAUGHT. Go's pdqsort falls back to
//     insertion sort below ~12 elements, so it is stable in practice at this fixture's size. The
//     comment on sortRows claiming stability is therefore unpinned by anything here.
//   - E6 — removing this statement's cross-workspace predicate is caught by OTHER tests in the
//     package and NOT by this one, which is the intended result: CAUGHT is not a catch-all.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

func TestListRows_TiedPositionsHaveADefinedOrder_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "rowtie-owner@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "S", Slug: "rowtie-s", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	pg, err := page.NewStore(d.Pool).Create(ctx, model.Page{
		SpaceID: sp.ID, WorkspaceID: W, Title: "P", CreatedBy: owner,
	})
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}

	chain := tierChain(d)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, tierReq(method, path, "rowtie-owner@corp.com", body))
		return rr.Code, strings.TrimSpace(rr.Body.String())
	}

	code, body := call(http.MethodPost, "/v1/pages/"+pg.ID+"/databases", `{"name":"Tasks"}`)
	if code != http.StatusCreated {
		t.Fatalf("create database: %d %s", code, body)
	}
	var dbObj struct{ ID string }
	if err := json.Unmarshal([]byte(body), &dbObj); err != nil {
		t.Fatalf("decode database: %v", err)
	}
	base := "/v1/databases/" + dbObj.ID

	// One text column, and every row carries it — `bucket` is deliberately the SAME value on the
	// two tied rows, so the view read further down exercises sortRows over an equal key.
	if c, b := call(http.MethodPatch, base+"/schema",
		`{"schema":[{"id":"c-title","name":"Title","type":"text"},`+
			`{"id":"c-bucket","name":"Bucket","type":"text"}]}`); c != http.StatusOK {
		t.Fatalf("set schema: %d %s", c, b)
	}

	newRow := func(title, bucket string, pos int) string {
		t.Helper()
		c, b := call(http.MethodPost, base+"/rows",
			`{"values":{"c-title":"`+title+`","c-bucket":"`+bucket+`"},"position":`+itoaTiny(pos)+`}`)
		if c != http.StatusCreated {
			t.Fatalf("create row %s: %d %s", title, c, b)
		}
		var row struct{ ID string }
		if err := json.Unmarshal([]byte(b), &row); err != nil {
			t.Fatalf("decode row %s: %v", title, err)
		}
		return row.ID
	}

	// ── The shipped sequence, in order. ────────────────────────────────────────────────
	newRow("alpha", "keep", 1)
	bravo := newRow("bravo", "keep", 2)
	newRow("charlie", "same", 3)

	if c, b := call(http.MethodDelete, base+"/rows/"+bravo, ""); c != http.StatusOK {
		t.Fatalf("delete bravo: %d %s", c, b)
	}
	// Autovacuum's job, done on demand so the moment is deterministic rather than whenever the
	// daemon next wakes. Without it the row below simply appends and the tie resolves in creation
	// order BY ACCIDENT — which is precisely the shape of a guard that cannot fail, so it is
	// forced here and the accident is denied.
	if _, err := d.Pool.Exec(ctx, `VACUUM database_rows`); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	// `rows.length + 1` with two rows left. Ties with charlie.
	newRow("delta", "same", 3)
	// Created LAST, sorts FIRST: the row that keeps `position` the primary key of the ordering.
	newRow("echo", "keep", 0)

	type apiRow struct {
		ID       string         `json:"id"`
		Position float64        `json:"position"`
		Values   map[string]any `json:"values"`
	}
	read := func(path string) []apiRow {
		t.Helper()
		c, b := call(http.MethodGet, path, "")
		if c != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, c, b)
		}
		var rows []apiRow
		if err := json.Unmarshal([]byte(b), &rows); err != nil {
			t.Fatalf("decode rows: %v (%s)", err, b)
		}
		return rows
	}
	titles := func(rows []apiRow) string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			s, _ := r.Values["c-title"].(string)
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}

	plain := read(base + "/rows")

	// ── [TIE-IS-REAL] the vacuity floor, asserted BEFORE the order is. ─────────────────
	// Everything below is a statement about two rows that share a position. If some future change
	// makes CreateRow hand out unique positions, the ordering assertions go green for a reason
	// that has nothing to do with the ORDER BY, and this repo would have a guard that cannot fail.
	// It is read back off the WIRE, not off the value posted.
	pos := map[string]float64{}
	for _, r := range plain {
		s, _ := r.Values["c-title"].(string)
		pos[s] = r.Position
	}
	if len(plain) != 4 {
		t.Fatalf("[TIE-IS-REAL] expected 4 rows, got %d (%s)", len(plain), titles(plain))
	}
	if pos["charlie"] != pos["delta"] {
		t.Fatalf("[TIE-IS-REAL] charlie and delta no longer share a position (%v vs %v) — the "+
			"ordering assertions below would be green without exercising the tiebreak",
			pos["charlie"], pos["delta"])
	}

	// ── [TIE-ORDER-IS-CREATION-ORDER] the finding. ────────────────────────────────────
	// charlie was created before delta. Before the tiebreak this came back
	// `echo,alpha,delta,charlie` — delta first, because it reused the heap slot bravo freed.
	if got := titles(plain); got != "echo,alpha,charlie,delta" {
		t.Errorf("[TIE-ORDER-IS-CREATION-ORDER] GET /rows = %q, want %q — two rows share "+
			"position %v and the read must not resolve that from the heap", got,
			"echo,alpha,charlie,delta", pos["charlie"])
	}

	// ── [POSITION-STILL-PRIMARY] the floor under the fix. ─────────────────────────────
	// echo was created LAST and carries position 0. A tiebreak that became the sort key — ORDER BY
	// created_at, or by id — puts it last and is caught here rather than passing as "deterministic".
	//
	// ⚠ NON-ISOLATING, MEASURED AND LABELLED RATHER THAN LEFT LOOKING LOAD-BEARING. Control E3
	// (`ORDER BY created_at ASC, id ASC` — the tiebreak eaten by the sort key) was PREDICTED to
	// redden this tag alone and reddens THREE: any reordering that moves echo also changes the
	// whole-list string, so [TIE-ORDER-IS-CREATION-ORDER] and [VIEW-SORT-KEEPS-IT] fire too. No
	// mutation in ~/talyvor-queue/w31-rowordertie-controls-5e91.py isolates it. It is kept because
	// it NAMES the reason a reader of a red run needs — "position is still the sort key" — but it
	// is not the assertion bearing that weight, and a future control should not be re-scoped until
	// it appears to be.
	if len(plain) > 0 {
		if first, _ := plain[0].Values["c-title"].(string); first != "echo" {
			t.Errorf("[POSITION-STILL-PRIMARY] first row is %q, want %q — position must still "+
				"order the list; created_at and id only break its ties", first, "echo")
		}
	}

	// ── [VIEW-SORT-KEEPS-IT] the Go half of the same read. ────────────────────────────
	// `sortRows` is a stable sort, so it PRESERVES the SQL order for rows whose sort key is equal —
	// which means an undefined SQL order stays undefined all the way to the client. charlie and
	// delta share `c-bucket`, so this asserts the tiebreak survives the in-process sort too. The
	// view sorts DESCENDING so its answer differs from the plain read above — an ascending one
	// would reproduce it exactly and assert nothing about the sort having run.
	c, b := call(http.MethodPost, base+"/views",
		`{"name":"By bucket","type":"table","sort_by":"c-bucket","sort_dir":"desc"}`)
	if c != http.StatusCreated {
		t.Fatalf("create view: %d %s", c, b)
	}
	var view struct{ ID string }
	if err := json.Unmarshal([]byte(b), &view); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	viewed := titles(read(base + "/rows?view_id=" + view.ID))
	if viewed != "charlie,delta,echo,alpha" {
		t.Errorf("[VIEW-SORT-KEEPS-IT] GET /rows?view_id = %q, want %q — the stable sort must "+
			"carry a DEFINED order into the tied bucket", viewed, "charlie,delta,echo,alpha")
	}
}
