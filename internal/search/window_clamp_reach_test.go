package search

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/page"
)

// THE type=all WINDOW CLAMP HAD NO TEST, AND MEASURING IT IS WHAT SHOWED IT DOES NOT HOLD AT THE
// TOP OF ITS OWN INPUT RANGE.
//
// `handler.go` computes `window = offset + limit` for type=all and then clamps it to maxFetchRows
// before multiplying by maxFetchFactor. Its comment states the purpose: "Clamped BEFORE the
// multiply so a caller-supplied offset cannot overflow the product."
//
// ⚠ THE SUM OVERFLOWS BEFORE THE CLAMP CAN SEE IT, AND `window > maxFetchRows` IS FALSE FOR A
// NEGATIVE NUMBER. Measured through this route: with limit=10 and offset=math.MaxInt64 the store
// was asked for a limit of -9223372036854775799. The clamp is not merely un-tested there, it is
// BYPASSED — which is the opposite of the "load-bearing guard with no test" this was filed as.
//
// ⚠ AND IT IS REACHABLE WITHOUT KNOWING MaxInt64. `offset, _ := strconv.Atoi(...)` DISCARDS the
// error, and Atoi returns MaxInt64 together with ErrRange for any longer run of digits — so
// `offset=99999999999999999999` lands on exactly the same path. Both are covered below.
//
// ⚠ WHAT THIS IS NOT: a live 500. Measured against real Postgres (pgvector/pgvector:pg16, the
// image CI uses), every offset below answers 200 with an empty result set, because BOTH stores
// independently correct a non-positive limit — page.Store.SearchWithRank and
// SemanticSearch.Search each carry `if limit <= 0 { limit = 10 }`. The store corrections are what
// keep a negative LIMIT away from Postgres today; the handler clamp gets the credit in the
// comments. That coupling is un-pinned and is filed separately rather than asserted here.
//
// ⚠ WHY THE ASSERTION IS ON THE ARGUMENT AND NOT ON THE STATUS: with the clamp gone the route
// still answers 200, so a status-only assertion is satisfied by the defect. The handler is mounted
// WITHOUT an access gate on purpose — that is the `fetchLimit = window` branch, so the limit the
// store receives IS the window, observed rather than recomputed.
type windowProbe struct {
	limit, offset int
	calls         int
}

func (p *windowProbe) SearchWithRank(_ context.Context, _, _ string, _ *string, limit, offset int) ([]page.SearchResult, error) {
	p.calls++
	p.limit, p.offset = limit, offset
	return nil, nil
}

// askWindow drives the real route and returns the limit and offset the store was handed.
func askWindow(t *testing.T, query string) (int, int, int) {
	t.Helper()
	probe := &windowProbe{}
	h := newRouter(t, probe, newSemanticSearch(lensintegration.New("", ""), nil))
	req := httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/search?q=auth+flow&"+query, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: status %d, want 200: %s", query, rr.Code, rr.Body.String())
	}
	// NON-VACUITY. A window assertion over a store that was never called proves nothing, and every
	// case below would pass on the zero value.
	if probe.calls != 1 {
		t.Fatalf("%s: store called %d times, want exactly 1 — the assertion below would be "+
			"measuring the zero value rather than the handler", query, probe.calls)
	}
	return probe.limit, probe.offset, rr.Code
}

func TestSearch_TypeAll_WindowClampBoundsWhatTheStoreIsAsked(t *testing.T) {
	const maxI64 = math.MaxInt64
	cases := []struct {
		name   string
		query  string
		want   int
		reason string
	}{
		// COUNTERWEIGHT FIRST. A clamp that pinned every window to maxFetchRows would satisfy
		// every over-the-bound case below; these two say the window still tracks the caller.
		{"below the bound", "type=all&limit=10&offset=0", 10,
			"offset 0 + limit 10 is under the ceiling and must be passed through, not pinned"},
		{"below the bound, paged", "type=all&limit=10&offset=10", 20,
			"the window is offset+limit for two sources, so page 2 asks for 20"},
		// EXACTLY AT THE BOUND IS ADMITTED, not refused. Without this a clamp written as `>=`
		// would pass every other case here.
		{"exactly at the bound", "type=all&limit=10&offset=40", maxFetchRows,
			"offset+limit == maxFetchRows must be admitted whole"},
		{"one past the bound", "type=all&limit=10&offset=41", maxFetchRows,
			"the first window the clamp actually has to cut"},
		{"far past the bound", "type=all&limit=10&offset=9000000000000000000", maxFetchRows,
			"a huge but non-overflowing offset — the clamp's ordinary job"},
		// THE OVERFLOW BAND. These are the cases that red before the sum is guarded.
		{"offset at MaxInt64", fmt.Sprintf("type=all&limit=10&offset=%d", maxI64), maxFetchRows,
			"offset+limit overflows to negative and `window > maxFetchRows` is false for it"},
		{"offset at MaxInt64, max limit", fmt.Sprintf("type=all&limit=50&offset=%d", maxI64), maxFetchRows,
			"the band widens with limit: any offset > MaxInt64-limit overflows"},
		{"offset past MaxInt64", "type=all&limit=10&offset=99999999999999999999", maxFetchRows,
			"strconv.Atoi returns MaxInt64 with ErrRange and the handler discards the error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := askWindow(t, tc.query)
			if got != tc.want {
				t.Fatalf("store asked for limit %d, want %d — %s", got, tc.want, tc.reason)
			}
			// THE INVARIANT, stated separately from the exact value: a window is a count of rows
			// and must be a usable one. -9223372036854775799 satisfies `<= maxFetchRows`, which is
			// why the upper bound alone is not the assertion.
			if got <= 0 || got > maxFetchRows {
				t.Fatalf("store asked for limit %d, which is not in (0, %d]", got, maxFetchRows)
			}
		})
	}
}

// SCOPE ARM. The single-source path deliberately does NOT go through the window: it pushes the
// caller's offset into SQL so deep paging keeps working. If the repair above had clamped `offset`
// itself rather than the window, this is what would have caught it.
func TestSearch_SingleSource_OffsetStillReachesTheStoreUnclamped(t *testing.T) {
	limit, offset, _ := askWindow(t, fmt.Sprintf("type=fulltext&limit=10&offset=%d", int(math.MaxInt64)))
	if limit != 10 {
		t.Fatalf("single-source limit = %d, want 10 — the window path must not apply here", limit)
	}
	if offset != math.MaxInt64 {
		t.Fatalf("single-source offset = %d, want MaxInt64 — the caller's offset is the SQL "+
			"offset for one source, and clamping it would break deep paging", offset)
	}
}
