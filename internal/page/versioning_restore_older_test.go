package page_test

// RESTORING AN OLDER VERSION — THE THING THE BUTTON IS FOR — WAS ASSERTED BY NOTHING.
//
// MEASURED, not inferred: a census of every `_test.go` in the repo at 78be685 found exactly two
// drivers of the restore path (`versioning_title_pairing_test.go` and the title-only guard added
// alongside it), and BOTH restore the NEWEST version and assert the live page did NOT change.
// Every assertion in the repo about RestoreVersion was therefore about a no-op. Deleting
// `"title": title` from the map RestoreVersion hands to Update — the restored column simply not
// being restored — left the full CI suite over all 42 packages GREEN (control E7,
// ~/talyvor-queue/w31-titleonly-controls-7e52.py).
//
// ⚠ THIS GUARD PASSED ON ITS FIRST RUN AND THAT IS EXPECTED, BECAUSE IT PINS A BEHAVIOUR THAT
// WORKS RATHER THAN FIXING ONE THAT DOES NOT. Red-first has no meaning for an absent assertion;
// what stands in for it is that the mutation which used to be invisible is now caught, both
// columns independently — see controls F1 and F2.
//
// Driven through the shipped /v1 chain (gatewayauth -> authz -> permission enforcer -> handler ->
// store), not the store alone: the enforcer requires AccessEdit for the restore route, and a
// store-level test would assert a write no caller can reach.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/testutil"
)

// [RESTORE-TITLE] / [RESTORE-CONTENT] — restoring an older version puts BOTH of that version's
// columns back on the live page.
//
// Both assertions use Errorf rather than Fatalf so a control verdict names WHICH column stopped
// being restored: one Fatalf would hide the second column behind the first.
func TestRestoreOlderVersion_WritesBothColumnsBack_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Original")
	sID := spaceOf(t, d, pID)
	ctx := t.Context()

	chain := newV1Chain(t, d)
	base := "/v1/spaces/" + sID + "/pages/" + pID

	// Two saves, each changing both versioned columns, so v1 and v2 differ in both.
	for _, s := range []struct{ title, content string }{
		{"A", `{"body":"1"}`},
		{"B", `{"body":"2"}`},
	} {
		body, err := json.Marshal(map[string]string{"title": s.title, "content": s.content})
		if err != nil {
			t.Fatalf("marshal save %s: %v", s.title, err)
		}
		if rr := patchAs(t, chain, base, "alice@corp.com", string(body)); rr.Code != http.StatusOK {
			t.Fatalf("save %s = %d: %s", s.title, rr.Code, rr.Body.String())
		}
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Gateway-Auth", testGatewaySecret)
		req.Header.Set("X-User-Email", "alice@corp.com")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		return rec
	}
	rec := get(base + "/versions")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET versions = %d: %s", rec.Code, rec.Body.String())
	}
	var vs []model.PageVersion
	if err := json.Unmarshal(rec.Body.Bytes(), &vs); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("ANCHOR: %d versions after two both-column saves, want 2 — there is no OLDER "+
			"version for this test to restore", len(vs))
	}
	// GetVersions is ORDER BY version DESC, so the oldest is last.
	older := vs[len(vs)-1]
	if older.Title != "A" || older.Content != `{"body":"1"}` {
		t.Fatalf("ANCHOR: the older version reads (title=%q, content=%q), want (A, body:1) — "+
			"restoring it could not tell a working restore from a broken one",
			older.Title, older.Content)
	}

	req := httptest.NewRequest(http.MethodPost,
		base+"/versions/"+strconv.Itoa(older.Version)+"/restore", nil)
	req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	req.Header.Set("X-User-Email", "alice@corp.com")
	rec = httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore v%d = %d: %s", older.Version, rec.Code, rec.Body.String())
	}

	var title, content string
	if err := d.Pool.QueryRow(ctx, `SELECT title, content FROM pages WHERE id=$1`, pID).
		Scan(&title, &content); err != nil {
		t.Fatalf("read live row: %v", err)
	}
	if title != "A" {
		t.Errorf("[RESTORE-TITLE] restoring v%d left the live title %q, want %q — the version's "+
			"title was not written back, so the restore silently did not restore",
			older.Version, title, "A")
	}
	if content != `{"body":"1"}` {
		t.Errorf("[RESTORE-CONTENT] restoring v%d left the live content %q, want %q",
			older.Version, content, `{"body":"1"}`)
	}
}
