package database_test

// THE `view_id` A CLIENT SENDS MUST REACH THE SQL — asserted through the production chain.
//
// `GET /v1/databases/{dbID}/rows?view_id=` is the only way a saved view's filters and sort are ever
// applied: the handler resolves the view out of ListViews and hands it to ListRows, which runs
// filterRows / sortRows over the fetched rows. Two things covered that engine before this file and
// neither covered the wire:
//
//   - store_test.go TestListRows_AppliesFilterAndSort passes the *DatabaseView VALUE straight to
//     ListRows, so it is green for any handler — including one that never reads the query
//     parameter at all.
//   - sec4_tier_test.go drives `GET …/rows` for the tier matrix and never sends a view_id.
//
// So `viewID := r.URL.Query().Get("view_id")` and everything downstream of it was asserted by
// nothing, on the same day the SPA was measured never to send it (frontend
// DatabaseBlock.view.test.tsx). A guard on the caller is worth little if the callee's half is
// unpinned.
//
// ⚠ MEASURED HERE FIRST, ON REAL POSTGRES, THROUGH gatewayauth + authz + the dbEnf tier gate:
//
//	GET /rows                -> A, B, <a row with no c-title cell>
//	GET /rows?view_id=V      -> B          (view: c-status neq done, sort c-title desc)
//	GET /rows?view_id=V      -> A          (same view switched to c-status eq done)
//
// ⚠ ONE THING THAT RUN SHOWED AND THIS FILE DELIBERATELY DOES NOT ASSERT: the third row above has
// no `c-status` cell at all — the row the SPA's "+ New" button creates (`values: {}`) — and it is
// absent from BOTH the `neq done` and the `eq done` answer. applyFilter returns false for a missing
// cell before the operator is ever consulted, so `eq X` and `neq X` are both false for the same
// row. Whether "is not done" should include an empty cell is a semantic call about the operator
// matrix, not a bug in this wiring, so the fixture below gives every row the filtered column and
// this file stays silent about it rather than pinning either answer as correct.

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

func TestListRows_TheViewIDOnTheRequestReachesTheSQL_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	W := d.Workspace(t)
	owner := d.Member(t, W, "viewrows-owner@corp.com")
	sp, err := space.NewStore(d.Pool).Create(ctx, model.Space{
		WorkspaceID: W, Name: "S", Slug: "viewrows-s", CreatedBy: owner,
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

	// The production chain, shared with sec4_tier_test.go / crossdatabase_realpg_test.go.
	chain := tierChain(d)
	call := func(method, path, body string) (int, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, tierReq(method, path, "viewrows-owner@corp.com", body))
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

	if c, b := call(http.MethodPatch, base+"/schema",
		`{"schema":[{"id":"c-status","name":"Status","type":"select","options":["todo","done"]},`+
			`{"id":"c-title","name":"Title","type":"text"}]}`); c != http.StatusOK {
		t.Fatalf("set schema: %d %s", c, b)
	}

	// Every row carries both cells — see the header for why the cell-less row is not in this
	// fixture. Positions are ascending so the unfiltered listing has a defined order.
	newRow := func(status, title string, pos int) {
		t.Helper()
		c, b := call(http.MethodPost, base+"/rows",
			`{"values":{"c-status":"`+status+`","c-title":"`+title+`"},"position":`+itoaTiny(pos)+`}`)
		if c != http.StatusCreated {
			t.Fatalf("create row %s: %d %s", title, c, b)
		}
	}
	newRow("done", "alpha", 1)
	newRow("todo", "bravo", 2)
	newRow("todo", "charlie", 3)

	c, b := call(http.MethodPost, base+"/views",
		`{"name":"Open","type":"table","filters":[{"col_id":"c-status","operator":"neq","value":"done"}],`+
			`"sort_by":"c-title","sort_dir":"desc"}`)
	if c != http.StatusCreated {
		t.Fatalf("create view: %d %s", c, b)
	}
	var view struct{ ID string }
	if err := json.Unmarshal([]byte(b), &view); err != nil {
		t.Fatalf("decode view: %v", err)
	}

	titles := func(path string) string {
		t.Helper()
		code, body := call(http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
		var rows []struct {
			Values map[string]any `json:"values"`
		}
		if err := json.Unmarshal([]byte(body), &rows); err != nil {
			t.Fatalf("decode rows: %v (%s)", err, body)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			s, _ := r.Values["c-title"].(string)
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}

	// ── [NO-VIEW] the unfiltered listing. This is the request the SPA made for the whole life of
	// the feature, and it is also the premise of [FILTERED]: without it a filtered answer could be
	// a database that simply has one row in it.
	if got := titles(base + "/rows"); got != "alpha,bravo,charlie" {
		t.Errorf("[NO-VIEW] a rows listing with no view_id returned %q, want %q — the unfiltered "+
			"read is the premise every assertion below rests on", got, "alpha,bravo,charlie")
	}

	// ── [FILTERED] the view's filter reached the answer. `c-status neq done` drops alpha.
	if got := titles(base + "/rows?view_id=" + view.ID); strings.Contains(got, "alpha") {
		t.Errorf("[FILTERED] GET /rows?view_id=%s returned %q — the saved view's filter did not "+
			"reach the read, so the view_id on the request bought nothing", view.ID, got)
	}

	// ── [SORTED] the view's sort reached the answer too, and it is a SEPARATE claim: a handler
	// that resolved the view for filtering and dropped it before the sort would satisfy [FILTERED]
	// alone. `sort_by c-title, sort_dir desc` over the two survivors is charlie,bravo — the
	// REVERSE of the position order [NO-VIEW] just pinned, so this cannot pass by accident.
	if got := titles(base + "/rows?view_id=" + view.ID); got != "charlie,bravo" {
		t.Errorf("[SORTED] GET /rows?view_id=%s returned %q, want %q — the saved view's sort_by/"+
			"sort_dir did not reach the read", view.ID, got, "charlie,bravo")
	}

	// ── [UNKNOWN-VIEW] a view_id that resolves to nothing must not narrow or empty the listing.
	// ListRows documents this ("a missing view is silently ignored — the rows still come back, just
	// unfiltered") and it is the branch a client sends after a view is deleted in another tab.
	if got := titles(base + "/rows?view_id=view-that-does-not-exist"); got != "alpha,bravo,charlie" {
		t.Errorf("[UNKNOWN-VIEW] an unresolvable view_id returned %q, want the full listing %q — "+
			"an unknown view must be ignored, not applied as an empty filter", got, "alpha,bravo,charlie")
	}
}

// itoaTiny keeps the row bodies above literal without pulling strconv in for three call sites.
func itoaTiny(i int) string {
	if i == 0 {
		return "0"
	}
	var out []byte
	for i > 0 {
		out = append([]byte{byte('0' + i%10)}, out...)
		i /= 10
	}
	return string(out)
}
