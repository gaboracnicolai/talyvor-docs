package analytics_test

// THE ANALYTICS SCREEN'S EMPTY STATE PUT `null` ON THE WIRE WHERE ITS OWN CLIENT TYPE DECLARES A
// REQUIRED ARRAY, AND THE SPA HAS NO ERROR BOUNDARY — SO THE WHOLE APP WENT BLANK.
//
// `WorkspaceReadStats.MostReadPages` / `.LeastReadPages` and `ReadStats.ViewsByDay` /
// `.TopViewers` are Go SLICES that GetWorkspaceStats / GetReadStats only ever `append` to. An
// unappended slice is nil, and `encoding/json` writes a nil slice as `null`, not `[]`.
//
// MEASURED ON REAL POSTGRES through the shipped routes at 366808c, raw response bytes:
//
//	GET /v1/workspaces/{ws}/analytics/pages        (brand-new workspace)
//	  {"total_views":0,"unique_viewers":0,"most_read_pages":null,"least_read_pages":null,
//	   "never_read_count":0}
//	GET /v1/workspaces/{ws}/analytics/pages        (one page, never viewed)
//	  … "most_read_pages":null,"least_read_pages":null,"never_read_count":1
//	GET /v1/spaces/{sp}/pages/{pg}/analytics       (page never viewed)
//	  {"page_id":"…","title":"","total_views":0,"unique_viewers":0,"avg_duration_sec":0,
//	   "views_by_day":null,"top_viewers":null}
//
// ⚠⚠ THE CONSUMER DEREFERENCES ALL FOUR BARE, AND THERE IS NO ErrorBoundary IN THE ENTIRE SPA
// (measured: 0 files match `ErrorBoundary|componentDidCatch|getDerivedStateFromError` under
// frontend/src). React 18 unmounts the ROOT on an uncaught render throw, so this is not a broken
// panel — it is a blank application. Rendered with the bytes above:
//
//	Workspace tab  TypeError: Cannot read properties of null (reading 'length')  Analytics.tsx:156
//	This-page tab  TypeError: Cannot read properties of null (reading 'map')     Analytics.tsx:70
//
// ⚠ IT IS NOT A NEW-WORKSPACE CORNER — THE SHIPPED ACCESS FILTER PRODUCES IT ON A BUSY ONE.
// GetWorkspaceStats builds the ranked cohorts by appending only the rows the caller MAY VIEW, so a
// member with no grant on the private spaces that hold the traffic gets nil cohorts however much
// readership the workspace has. [FILTERED-TO-EMPTY] is that case, and it is the reason this is a
// defect on a live product rather than on an onboarding screen.
//
// ⚠⚠ WHY THE EXISTING REAL-PG TESTS IN THIS PACKAGE COULD NOT SEE IT, WHICH IS THE HALF WORTH
// KEEPING: privatespace_realpg_test.go and templatecohort_realpg_test.go both assert through
// `json.Unmarshal([]byte(body), &out)` into `analytics.WorkspaceReadStats`. `null` and `[]` both
// decode to a `[]ReadStats` of length 0 — they are INDISTINGUISHABLE after decoding. Every
// assertion those files make is true under both wire shapes, so a decoded-struct assertion is
// blind to this class BY CONSTRUCTION. THIS FILE ASSERTS ON THE RAW BYTES, and that is the only
// reason it can fail.
//
// THE CASES:
//
//	[EMPTY-WS-COHORTS]  new workspace           both cohorts must be `[]`            ← the defect
//	[NEVER-READ-PAGE]   page with no views      both lists must be `[]`              ← the defect
//	[FILTERED-TO-EMPTY] traffic, no grant       cohorts emptied by the gate are `[]` ← the defect
//	[RANKED-ROW-LISTS]  traffic, visible        each ranked ROW's lists are `[]`     ← the defect
//	[POPULATED]         traffic, visible        the cohorts still carry the row      ← positive control
//	[VIEWED-PAGE-LISTS] page with views         the lists still carry their rows     ← positive control
//
// The two positive controls are why "return `[]` for everything" is not a passing fix.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// rawField returns the UNDECODED bytes of one top-level field. Decoding into the typed struct is
// what hid this class; this is the instrument that can see it.
func rawField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, body)
	}
	v, ok := m[field]
	if !ok {
		t.Fatalf("field %q absent from the response: %s", field, body)
	}
	return strings.TrimSpace(string(v))
}

// mustJSONArray fails when the field is anything other than a JSON array — `null` included.
func mustJSONArray(t *testing.T, tag, body, field string) []json.RawMessage {
	t.Helper()
	raw := rawField(t, body, field)
	if !strings.HasPrefix(raw, "[") {
		t.Errorf("%s %q is %s on the wire, want a JSON array — the client type declares it a required array and dereferences it bare, and the SPA has no error boundary\nbody: %s",
			tag, field, raw, body)
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatalf("%s decode %q: %v", tag, field, err)
	}
	return rows
}

func TestAnalyticsEmptyListsAreArrays_RealPG(t *testing.T) {
	d := testutil.New(t)
	f := newAnalyticsFixture(t, d)
	rollupPath := "/v1/workspaces/" + f.ws + "/analytics/pages"

	// ── [EMPTY-WS-COHORTS] a workspace with no spaces, no pages and no views ──────────
	code, body := f.get(t, "alice@example.com", f.alice, rollupPath)
	if code != http.StatusOK {
		t.Fatalf("[EMPTY-WS-COHORTS] rollup = %d, want 200: %s", code, body)
	}
	mustJSONArray(t, "[EMPTY-WS-COHORTS]", body, "most_read_pages")
	mustJSONArray(t, "[EMPTY-WS-COHORTS]", body, "least_read_pages")

	// ── [NEVER-READ-PAGE] a page that exists and has never been viewed ───────────────
	pub := seedSpaceA(t, d, f.ws, f.alice, "public", false)
	unread := seedPageA(t, d, f.ws, pub, f.alice, "Never Read Doc")
	code, body = f.get(t, "alice@example.com", f.alice, "/v1/spaces/"+pub+"/pages/"+unread+"/analytics")
	if code != http.StatusOK {
		t.Fatalf("[NEVER-READ-PAGE] page stats = %d, want 200: %s", code, body)
	}
	mustJSONArray(t, "[NEVER-READ-PAGE]", body, "views_by_day")
	mustJSONArray(t, "[NEVER-READ-PAGE]", body, "top_viewers")

	// ── [FILTERED-TO-EMPTY] a workspace with real traffic, all of it in a private space
	// bob holds no grant on. The rows exist and rank; the visibility filter drops every one,
	// so the cohorts are built by appending nothing — the same nil, on a busy workspace.
	priv := seedSpaceA(t, d, f.ws, f.alice, "private", true)
	secret := seedPageA(t, d, f.ws, priv, f.alice, "Q3 Layoff Plan")
	seedViewsA(t, d, f.ws, secret, f.alice, 7)
	code, body = f.get(t, "bob@example.com", f.bob, rollupPath)
	if code != http.StatusOK {
		t.Fatalf("[FILTERED-TO-EMPTY] rollup as bob = %d, want 200: %s", code, body)
	}
	if strings.Contains(body, "Q3 Layoff Plan") {
		t.Fatalf("[FILTERED-TO-EMPTY] premise broken — bob can see the private page: %s", body)
	}
	most := mustJSONArray(t, "[FILTERED-TO-EMPTY]", body, "most_read_pages")
	mustJSONArray(t, "[FILTERED-TO-EMPTY]", body, "least_read_pages")
	if len(most) != 0 {
		t.Fatalf("[FILTERED-TO-EMPTY] premise broken — expected the gate to empty the cohort, got %d rows: %s", len(most), body)
	}

	// ── [POPULATED] + [RANKED-ROW-LISTS] the same rollup for a caller who CAN see traffic.
	// alice created the private space, so both pages are hers to read.
	seedViewsA(t, d, f.ws, unread, f.alice, 2)
	code, body = f.get(t, "alice@example.com", f.alice, rollupPath)
	if code != http.StatusOK {
		t.Fatalf("[POPULATED] rollup as alice = %d, want 200: %s", code, body)
	}
	for _, field := range []string{"most_read_pages", "least_read_pages"} {
		rows := mustJSONArray(t, "[RANKED-ROW-LISTS]", body, field)
		// POSITIVE CONTROL: emptying the cohorts is not a fix.
		if len(rows) == 0 {
			t.Fatalf("[POPULATED] %s is empty for a caller who can read both viewed pages: %s", field, body)
		}
		for i, row := range rows {
			mustJSONArray(t, "[RANKED-ROW-LISTS] row "+itoa(i)+" of "+field, string(row), "views_by_day")
			mustJSONArray(t, "[RANKED-ROW-LISTS] row "+itoa(i)+" of "+field, string(row), "top_viewers")
		}
	}

	// ── [VIEWED-PAGE-LISTS] positive control on the per-page route: a page WITH views must
	// still carry its rows, so "always emit []" fails here.
	code, body = f.get(t, "alice@example.com", f.alice, "/v1/spaces/"+pub+"/pages/"+unread+"/analytics")
	if code != http.StatusOK {
		t.Fatalf("[VIEWED-PAGE-LISTS] page stats = %d, want 200: %s", code, body)
	}
	days := mustJSONArray(t, "[VIEWED-PAGE-LISTS]", body, "views_by_day")
	viewers := mustJSONArray(t, "[VIEWED-PAGE-LISTS]", body, "top_viewers")
	if len(days) == 0 || len(viewers) == 0 {
		t.Fatalf("[VIEWED-PAGE-LISTS] a page with 2 views reported %d day-buckets and %d viewers: %s",
			len(days), len(viewers), body)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
