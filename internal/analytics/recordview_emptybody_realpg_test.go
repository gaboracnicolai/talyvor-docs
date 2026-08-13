package analytics_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/testutil"
)

// THE SERVER HALF OF THE DEAD VIEW RECORD — THE CONTRACT `PageView.viewrecord.wire.test.tsx`
// ASSERTS AGAINST, MEASURED HERE RATHER THAN ASSUMED THERE.
//
// The SPA opened every document with `pagesApi.recordView(space.id, page.id)` — `{ method:
// "POST" }` and NO body, because `api/client.ts` only sets `init.body` when the caller passes
// one. This file drives the SHIPPED route (the same chi chain main.go builds, via chainBoth)
// with exactly the three bodies that matter and reads the DATABASE for the answer, so "the
// view was recorded" is a row count rather than a status code.
//
// It pins BOTH ways a view POST can record nothing, because they fail differently and only one
// of them is visible:
//
//	A. EMPTY BODY   → 400. handler.go RecordView's first statement is
//	                  `if err := json.NewDecoder(r.Body).Decode(&in); err != nil` and an empty
//	                  body decodes to io.EOF. Nothing downstream runs.
//	B. duration < 3 → 200 {"ok":true} AND NO ROW. store.go's `if view.Duration < minDuration
//	                  { return nil }` discards it before the INSERT and before the
//	                  `pages.view_count` bump, and returns a nil error the handler reports as
//	                  success.
//
// ⚠ B IS WHY "MAKE THE HANDLER TOLERATE AN EMPTY BODY" IS THE WRONG FIX FOR A. A tolerant
// decode turns the bodyless open-a-document POST from a visible 400 into case B — a silent
// discard the client is told succeeded. That is a strictly worse failure, and it is the reason
// the frontend guard asserts `duration_sec >= 3` rather than merely "a body was sent".
//
// ⚠ WHAT THIS FILE DOES NOT CLAIM: it does not assert the SPA sends an empty body — that is a
// frontend fact and the frontend guard measures it off a stubbed fetch. This one only fixes
// what the server does with each shape, so neither side has to take the other's word for it.
func TestRecordView_EmptyBodyAndSubThresholdRecordNothing_RealPG(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()

	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Spec")
	var spaceID string
	if err := d.Pool.QueryRow(ctx, `SELECT space_id FROM pages WHERE id=$1`, pageID).Scan(&spaceID); err != nil {
		t.Fatal(err)
	}

	chain := chainBoth(d)
	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()
		var r io.Reader = http.NoBody
		if body != "" {
			r = strings.NewReader(body)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/spaces/"+spaceID+"/pages/"+pageID+"/view", r)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gateway-Auth", anSecret)
		req.Header.Set("X-User-Email", "alice@corp.com")
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, req)
		return rr
	}

	// ── A. the exact request the shipped SPA sent on every page open ──────────────────────
	rr := post(t, "")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("[EMPTY-BODY-REJECTED] bodyless POST .../view = %d, want 400. body=%s",
			rr.Code, rr.Body.String())
	}
	if got := pageViewsCount(t, d, pageID); got != 0 {
		t.Errorf("[EMPTY-BODY-REJECTED] bodyless POST inserted %d page_views rows, want 0", got)
	}
	if got := viewCount(t, d, pageID); got != 0 {
		t.Errorf("[EMPTY-BODY-REJECTED] bodyless POST bumped pages.view_count to %d, want 0", got)
	}

	// ── B. a body that decodes fine and is still discarded, with a 200 ────────────────────
	rr = post(t, `{"viewer_name":"Alice","duration_sec":2}`)
	if rr.Code != http.StatusOK {
		t.Errorf("[SUB-THRESHOLD-SILENT] 2s POST .../view = %d, want 200 (this is the point: it "+
			"reports success). body=%s", rr.Code, rr.Body.String())
	}
	if got := pageViewsCount(t, d, pageID); got != 0 {
		t.Errorf("[SUB-THRESHOLD-SILENT] a 2s view was RECORDED (%d rows) — minDuration no longer "+
			"drops sub-threshold views, so the frontend guard's >=3s assertion is now stricter "+
			"than the server and should be re-derived from store.go rather than left here", got)
	}
	if got := viewCount(t, d, pageID); got != 0 {
		t.Errorf("[SUB-THRESHOLD-SILENT] a 2s view bumped pages.view_count to %d, want 0", got)
	}

	// ── C. the only shape that records anything — the flush the screen keeps ──────────────
	rr = post(t, `{"viewer_name":"Alice","duration_sec":10}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("[THRESHOLD-VIEW-RECORDED] 10s POST .../view = %d, want 200. body=%s",
			rr.Code, rr.Body.String())
	}
	if got := pageViewsCount(t, d, pageID); got != 1 {
		t.Fatalf("[THRESHOLD-VIEW-RECORDED] page_views rows after a 10s view = %d, want exactly 1. "+
			"With A and B both recording nothing, this is the ONLY path left that records a view "+
			"at all — if it is 0 the whole readership surface is a structural zero", got)
	}
	if got := viewCount(t, d, pageID); got != 1 {
		t.Errorf("[THRESHOLD-VIEW-RECORDED] pages.view_count after a 10s view = %d, want 1", got)
	}
	var duration int
	if err := d.Pool.QueryRow(ctx,
		`SELECT duration_sec FROM page_views WHERE page_id=$1`, pageID).Scan(&duration); err != nil {
		t.Fatal(err)
	}
	if duration != 10 {
		t.Errorf("[THRESHOLD-VIEW-RECORDED] page_views.duration_sec = %d, want 10 — the value the "+
			"AVG(duration_sec) behind avg_duration_sec is computed from", duration)
	}
}
