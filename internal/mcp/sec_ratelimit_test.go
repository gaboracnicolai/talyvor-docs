package mcp_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/gatewayauth"
	"github.com/talyvor/docs/internal/mcp"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/ratelimit"
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// MCP is the 7th LLM surface: the `ask_docs` tool reaches ai.Engine.AskDocs → Lens, on the
// same unmetered service key as the REST routes, and an agent loop can call it far faster
// than a human clicks. Rate-limiting the /mcp ENDPOINT wholesale would be wrong — it is one
// JSON-RPC door for 10 tools, 9 of which never touch an LLM — so the limit belongs at the
// chokepoint, applied only to the LLM tools.
//
// The key must be the VERIFIED workspace. callTool already resolves it
// (AuthorizeWorkspace → WithAuthorized) before dispatch, so the limiter keys on that
// Membership, never on the client-supplied workspace_id arg — the same discipline as the
// REST middleware and #23.

// mcpLimitChain mirrors newMCPChain but wires a limiter.
func mcpLimitChain(t *testing.T, d *testutil.DB, l *ratelimit.Limiter) http.Handler {
	t.Helper()
	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test").WithRateLimit(l)
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Post("/mcp", srv.HandleRPC)
	})
	return r
}

// rpcErrCode digs the JSON-RPC error code out of a tools/call response, or 0 when the call
// succeeded. HandleRPC returns 200 with an error object rather than an HTTP status.
func rpcErrCode(t *testing.T, body string) int {
	t.Helper()
	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode rpc response: %v (body=%s)", err, body)
	}
	if resp.Error == nil {
		return 0
	}
	return resp.Error.Code
}

func TestSecMCP_AskDocs_RateLimitedPerVerifiedWorkspace(t *testing.T) {
	d := testutil.New(t)

	wsA := d.Workspace(t)
	alice := d.Member(t, wsA, "alice@corp.com")
	d.Page(t, wsA, alice, "Spec")
	wsB := d.Workspace(t)
	bob := d.Member(t, wsB, "bob@corp.com")
	d.Page(t, wsB, bob, "B spec")

	l := ratelimit.New(60, 2) // burst 2
	chain := mcpLimitChain(t, d, l)

	ask := func(email, ws string) int {
		rr := callTool(chain, email, true, "ask_docs", map[string]any{
			"question": "what is the deployment process?", "workspace_id": ws,
		})
		return rpcErrCode(t, rr.Body.String())
	}

	// Burst is available.
	for i := 1; i <= 2; i++ {
		if code := ask("alice@corp.com", wsA); code != 0 {
			t.Fatalf("ask_docs %d/2 returned rpc error %d, want success (inside the burst)", i, code)
		}
	}
	// The 3rd is throttled — an agent loop cannot drive unbounded Lens spend.
	if code := ask("alice@corp.com", wsA); code == 0 {
		t.Error("ask_docs call 3 succeeded past the burst — the MCP LLM tool is unthrottled, so an " +
			"agent loop drives unbounded Lens spend on Docs's service key")
	}

	// PER-TENANT ISOLATION: B's bucket is untouched by A exhausting its own.
	for i := 1; i <= 2; i++ {
		if code := ask("bob@corp.com", wsB); code != 0 {
			t.Errorf("ws B ask_docs %d/2 returned rpc error %d — A exhausting ITS bucket must not "+
				"throttle B", i, code)
		}
	}
}

// A NON-LLM tool must not be throttled by the LLM limiter: /mcp is one door for 10 tools,
// and get_page never touches Lens.
func TestSecMCP_NonLLMToolsNotThrottled(t *testing.T) {
	d := testutil.New(t)
	ws := d.Workspace(t)
	alice := d.Member(t, ws, "alice@corp.com")
	pageID := d.Page(t, ws, alice, "Spec")

	l := ratelimit.New(60, 1) // burst 1 — one ask_docs and the bucket is dry
	chain := mcpLimitChain(t, d, l)

	// Drain the LLM bucket.
	callTool(chain, "alice@corp.com", true, "ask_docs", map[string]any{
		"question": "q", "workspace_id": ws,
	})

	// get_page is not an LLM tool and must still work.
	for i := 1; i <= 5; i++ {
		rr := callTool(chain, "alice@corp.com", true, "get_page", map[string]any{"page_id": pageID})
		if code := rpcErrCode(t, rr.Body.String()); code != 0 {
			t.Fatalf("get_page %d returned rpc error %d — a non-LLM tool must not consume or be "+
				"blocked by the LLM rate limit", i, code)
		}
		if !strings.Contains(rr.Body.String(), "Spec") {
			t.Fatalf("get_page %d did not return the page: %s", i, rr.Body.String())
		}
	}
}
