package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// SEC-4 MCP door: /mcp is on the root router, outside /v1, with NO auth (HandleRPC decodes
// JSON-RPC and dispatches — no transit proof, no identity). Tools take workspace_id / page_id
// as JSON-RPC ARGS and call unscoped stores, so any caller can pull any workspace's page
// content by id — the arg-trust IDOR class.
//
// This test demonstrates the leak. It is SKIPPED pending an MCP auth-model decision: MCP is an
// agent/service surface, not a gateway user surface, so bolting the user-membership boundary
// onto it (gatewayauth + x-user-email) would break every agent client. See the PR report — the
// recommended fix is a service-token + workspace pin, which is the user's call. When that lands,
// remove the Skip and this must pass.
func TestSEC4_MCP_ArgTrust_CrossTenant(t *testing.T) {
	t.Skip("BLOCKED on MCP auth-model decision — see PR report (door inventory, MCP). Demonstrates the arg-trust leak; un-skip when MCP gains service-token auth + workspace scoping.")

	d := testutil.New(t)
	ctx := context.Background()
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	pB := d.Page(t, wsB, bob, "B Secret")
	if _, err := d.Pool.Exec(ctx, `UPDATE pages SET content_text=$1 WHERE id=$2`, "TOPSECRET-B-CONTENT", pB); err != nil {
		t.Fatal(err)
	}

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test")
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_page","arguments":{"page_id":%q}}}`, pB)
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)))

	// An unauthenticated MCP caller must NOT receive another workspace's page content.
	if strings.Contains(rr.Body.String(), "TOPSECRET-B-CONTENT") {
		t.Errorf("MCP arg-trust LEAK: get_page returned B's content to an unauthenticated caller:\n%s", rr.Body.String())
	}
}
