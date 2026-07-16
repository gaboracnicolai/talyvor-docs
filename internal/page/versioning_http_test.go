package page_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/testutil"
)

// PHASE 1 — the get-one + diff endpoints work end-to-end through the real /v1 chain (gateway →
// authz → permission enforcer → handler → store) for the page's own member.
func TestVersions_OwnerCanGetAndDiff_RealPG(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pID := d.Page(t, ws, alice, "Alice's doc")
	sID := spaceOf(t, d, pID)

	store := page.NewStore(d.Pool)
	ctx := context.Background()
	if _, err := store.Update(ctx, pID, map[string]any{"content": `{"rev":1}`, "updated_by": alice}); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if _, err := store.Update(ctx, pID, map[string]any{"content": `{"rev":2}`, "updated_by": alice}); err != nil {
		t.Fatalf("save v2: %v", err)
	}

	chain := newV1Chain(t, d)
	do := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		chain.ServeHTTP(rr, asUser(http.MethodGet, path, "alice@corp.com", true, nil))
		return rr
	}
	base := "/v1/spaces/" + sID + "/pages/" + pID

	// get-one
	rr := do(base + "/versions/1")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET version 1 = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var v model.PageVersion
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode version: %v", err)
	}
	if v.Version != 1 || v.WorkspaceID != ws || !strings.Contains(v.Content, `"rev":1`) {
		t.Fatalf("version 1 = %+v, want {version:1 ws:%s content~rev:1}", v, ws)
	}

	// diff two
	rr = do(base + "/versions/1/diff/2")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET diff 1..2 = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var diff struct {
		From model.PageVersion `json:"from"`
		To   model.PageVersion `json:"to"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &diff); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if diff.From.Version != 1 || diff.To.Version != 2 {
		t.Fatalf("diff = (%d,%d), want (1,2)", diff.From.Version, diff.To.Version)
	}
	if !strings.Contains(diff.From.Content, `"rev":1`) || !strings.Contains(diff.To.Content, `"rev":2`) {
		t.Fatalf("diff content mismatch: from=%q to=%q", diff.From.Content, diff.To.Content)
	}
}
