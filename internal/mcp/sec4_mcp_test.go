package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

const testGatewaySecret = "sec4-test-gateway-secret-0123456789"

// SEC-4 MCP door: /mcp tools take workspace_id / page_id / space_id as JSON-RPC args and (pre-fix)
// call unscoped stores with NO auth — any caller pulls any workspace's content by id (arg-trust).
// Fix (model b, mirroring Track): /mcp behind gatewayauth + authz, and a per-tool chokepoint
// resolves each tool's acted-on workspace and authorizes it against the VERIFIED caller's
// memberships before dispatch.
//
// RED: newMCPChain mounts /mcp on the root router with no middleware and callTool has no
// chokepoint → (a)(b)(c) below FAIL. GREEN: /mcp is gated + chokepoint added → same asserts pass.
func newMCPChain(t *testing.T, d *testutil.DB) http.Handler {
	t.Helper()
	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test")
	r := chi.NewRouter()
	// GREEN wiring, mirroring main.go: /mcp behind gatewayauth + authz (model b). (Pre-fix this
	// mounted on the root router with no middleware — the RED baseline.)
	r.Group(func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Post("/mcp", srv.HandleRPC)
	})
	return r
}

// callTool posts a JSON-RPC tools/call and returns the recorder. withProof=false drops the
// transit proof; email sets the verified identity header.
func callTool(chain http.Handler, email string, withProof bool, tool string, args map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if withProof {
		req.Header.Set("X-Gateway-Auth", testGatewaySecret)
	}
	if email != "" {
		req.Header.Set("X-User-Email", email)
	}
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	return rr
}

func TestSEC4_MCP_ArgTrust_CrossTenant(t *testing.T) {
	d := testutil.New(t)
	ctx := context.Background()
	wsA := d.Workspace(t)
	d.Member(t, wsA, "alice@corp.com") // Alice → A only
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pB := d.Page(t, wsB, bob, "B Secret")
	if _, err := d.Pool.Exec(ctx, `UPDATE pages SET content_text=$1 WHERE id=$2`, "TOPSECRET-B-CONTENT", pB); err != nil {
		t.Fatal(err)
	}
	chain := newMCPChain(t, d)
	leaks := func(rr *httptest.ResponseRecorder) bool {
		return strings.Contains(rr.Body.String(), "TOPSECRET-B-CONTENT")
	}
	denied := func(rr *httptest.ResponseRecorder) bool {
		b := rr.Body.String()
		return strings.Contains(b, "not a member") || strings.Contains(b, "not authorized")
	}

	// (a) NO-AUTH: get_page for B's page with no transit proof → must be 401 (before dispatch).
	if rr := callTool(chain, "alice@corp.com", false, "get_page", map[string]any{"page_id": pB}); rr.Code != http.StatusUnauthorized {
		t.Errorf("(a) no-auth get_page = %d, want 401; leaked=%v", rr.Code, leaks(rr))
	}

	// (b) ARG-TRUST get_page: Alice (member A) supplying B's page_id → denied, no content.
	if rr := callTool(chain, "alice@corp.com", true, "get_page", map[string]any{"page_id": pB}); leaks(rr) || !denied(rr) {
		t.Errorf("(b) Alice get_page B: leaked=%v denied=%v — want denied, no content:\n%s", leaks(rr), denied(rr), rr.Body.String())
	}

	// (c) ARG-TRUST search/list: Alice supplying workspace B → denied.
	if rr := callTool(chain, "alice@corp.com", true, "search_docs", map[string]any{"query": "secret", "workspace_id": wsB}); !denied(rr) {
		t.Errorf("(c) Alice search_docs workspace_id=B not denied:\n%s", rr.Body.String())
	}

	// (d) SAME-WORKSPACE: Bob (member B) get_page P_B → content returned; proves we gated
	// cross-tenant, not all access.
	if rr := callTool(chain, "bob@corp.com", true, "get_page", map[string]any{"page_id": pB}); !leaks(rr) {
		t.Errorf("(d) Bob get_page B (own) returned no content — over-blocked:\n%s", rr.Body.String())
	}
	if rr := callTool(chain, "bob@corp.com", true, "search_docs", map[string]any{"query": "secret", "workspace_id": wsB}); denied(rr) {
		t.Errorf("(d) Bob search_docs workspace_id=B denied — over-blocked:\n%s", rr.Body.String())
	}
}

// STEP 4 fail-closed cases: a missing/nonexistent object (not a full-table leak) and a
// verified caller with NO memberships both deny.
func TestSEC4_MCP_FailClosed(t *testing.T) {
	d := testutil.New(t)
	wsB := d.Workspace(t)
	d.Member(t, wsB, "bob@corp.com")
	chain := newMCPChain(t, d)
	denied := func(rr *httptest.ResponseRecorder) bool {
		b := rr.Body.String()
		return strings.Contains(b, "not a member") || strings.Contains(b, "not authorized")
	}

	// nonexistent page_id, even as a real member (Bob) → deny (object→workspace unresolvable),
	// never a leak or a full-table scan.
	if rr := callTool(chain, "bob@corp.com", true, "get_page", map[string]any{"page_id": "does-not-exist"}); !denied(rr) {
		t.Errorf("nonexistent page_id not denied (fail-closed):\n%s", rr.Body.String())
	}
	// verified caller with NO membership rows → denied on every workspace.
	if rr := callTool(chain, "nobody@corp.com", true, "search_docs", map[string]any{"query": "x", "workspace_id": wsB}); !denied(rr) {
		t.Errorf("no-membership caller not denied:\n%s", rr.Body.String())
	}
}
