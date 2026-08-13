package mcp_test

// THREE GUARDS IN internal/mcp THAT CANNOT FIRE, AND THE ONE THAT CRASHES.
//
// mcp.New takes CONCRETE POINTERS (*page.Store, *space.Store, *analytics.Store, *ai.Engine,
// *freshness.FreshnessEngine) and assigns each into an INTERFACE field on deps. A nil pointer
// stored in an interface produces a NON-NIL interface holding a nil pointer, so every
// `s.deps.X == nil` / `!= nil` test in server.go asks a question whose answer is fixed for
// anything New builds:
//
//	server.go  if s.deps.spaces != nil    → always TRUE  → calls GetByID on a nil *space.Store
//	server.go  if s.deps.ai != nil        → always TRUE  (documented there; behaviour is correct)
//	server.go  if s.deps.freshness == nil → always FALSE (behaviour is correct via the engine)
//
// This is the SAME footgun freshness.GetStaleReport records as a measured SIGSEGV
// ("mcp.New(..., nil, \"test\") + get_stale_pages = SIGSEGV"), answered there with a
// nil-RECEIVER check, and whose comment hands the remaining half on:
// "⚠ THE DEAD nil GUARD IN internal/mcp IS ITS OWN FINDING AND IS NOT FIXED HERE."
//
// ⚠ WHY THE FIX IS AT New AND NOT AT THE THREE CALL SITES. Normalising a nil pointer to a
// genuine nil interface at the ONE constructor makes all three guards live at once, and a
// fourth dep added later inherits it. But making them live is NOT free, and that is the real
// content of this file: TWO OF THE THREE FALLBACKS ARE LIES THAT ONLY THE DEAD GUARD WAS
// HIDING. get_stale_pages answers a genuinely-absent engine with `[]` — the positive claim
// "nothing in this workspace needs attention", which internal/freshness rejects in its own words
// ("An empty stale report is not a neutral response"). ask_docs answers a genuinely-absent engine
// with `answer: ""` beside the sources it did find — "I read these documents and they say
// nothing", the exact shape server.go says was fixed. So the guards and their fallbacks move
// together, or fixing the class makes it worse.
//
// RED/GREEN, stated per test, because two of these three PASS BEFORE THE FIX and their whole
// value is as controls on it:
//   - NilSpaceStore_GetPage        RED before (panic), GREEN after.
//   - NilFreshness_GetStalePages   passes before AND after; the regression control that stops the
//     New change from turning a dead guard into a live lie.
//   - NilAI_AskDocs                same shape, same reason.

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
	"github.com/talyvor/docs/internal/space"
	"github.com/talyvor/docs/internal/testutil"
)

// chainForServer mounts one server behind the same gatewayauth+authz chain main.go uses, so
// these tests exercise the shipped door rather than calling tool functions directly.
func chainForServer(d *testutil.DB, srv *mcp.Server) http.Handler {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		exempt := func(string) bool { return false }
		r.Use(gatewayauth.Middleware(testGatewaySecret, exempt))
		r.Use(authz.Middleware(authz.NewPGResolver(d.Pool), exempt))
		r.Post("/mcp", srv.HandleRPC)
	})
	return r
}

// rpcEnvelope decodes a tools/call reply into (result, error). Exactly one is non-nil.
func rpcEnvelope(t *testing.T, body []byte) (map[string]any, map[string]any) {
	t.Helper()
	var env struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode rpc envelope: %v (body=%s)", err, body)
	}
	return env.Result, env.Error
}

// contentText pulls the single text payload out of a toolContent result.
func contentText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("[NO-CONTENT] result carried no content: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// RED BEFORE THE FIX: get_page on a server built by New with a nil *space.Store panics with a nil
// pointer dereference inside space.Store.GetByID (which dereferences s.pool with no receiver
// check), because the `if s.deps.spaces != nil` meant to skip exactly that call is TRUE for a
// typed nil. GREEN AFTER: the guard is live, the space name is simply absent, and the page — the
// thing the tool was asked for — is still served.
func TestMCP_NilSpaceStore_GetPageDoesNotPanic(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	pgID := d.Page(t, W, owner, "Runbook")

	srv := mcp.New(page.NewStore(d.Pool), nil, nil, nil, nil, "test").WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("[PANIC] get_page panicked with a nil *space.Store: %v\n"+
				"the `s.deps.spaces != nil` guard cannot fire — New stores a nil pointer in an "+
				"interface field, so the guard is true and GetByID runs on a nil receiver", rec)
		}
	}()

	rr := callTool(chain, "owner@corp.com", true, "get_page", map[string]any{"page_id": pgID})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		t.Fatalf("[TOOL-ERROR] get_page errored with a nil space store: %v", errObj)
	}
	text := contentText(t, result)
	// [PAGE-SERVED] — the absence assertions below are only meaningful if the tool actually
	// answered. Without this a blinded get_page would satisfy "did not panic" vacuously.
	if !strings.Contains(text, "Runbook") {
		t.Errorf("[PAGE-SERVED] get_page did not carry the page title: %s", text)
	}
}

// CONTROL (passes before AND after): a genuinely-absent freshness engine must NOT be answered
// with an empty stale list. `[]` is the positive claim "nothing in this workspace needs
// attention" — internal/freshness returns errors rather than empty lists for exactly this
// reason, and the SPA paints the empty list as a zero on the sidebar. Before the fix the dead
// guard routes through the engine's nil-receiver check and yields ErrNoPageReadGate; after the
// fix the guard is live and must not start returning [] instead.
func TestMCP_NilFreshness_GetStalePagesIsNotAnEmptyList(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	d.Member(t, W, "owner@corp.com")

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "get_stale_pages", map[string]any{"workspace_id": W})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		return // an error is the correct answer — nothing further to assert
	}
	// [STALE-NOT-EMPTY] an unwired engine answered with a list. Any list is wrong here, but the
	// empty one is the dangerous one: it reads as a measured "all fresh".
	t.Errorf("[STALE-NOT-EMPTY] get_stale_pages with NO freshness engine returned a RESULT, not "+
		"an error: %s\nan absent engine cannot report that a workspace is clean", contentText(t, result))
}

// CONTROL (passes before AND after): a genuinely-absent AI engine must NOT be answered with
// `answer: ""`. server.go records why — an empty answer beside real sources reads as "I read
// these documents and they say nothing", a claim about the documents rather than about the
// service. Before the fix the typed nil reaches ai.Engine's nil-receiver IsAvailable and returns
// ErrUnavailable; after the fix the live guard must not skip the call and return "" instead.
func TestMCP_NilAI_AskDocsIsNotAnEmptyAnswer(t *testing.T) {
	d := testutil.New(t)
	W := d.Workspace(t)
	owner := d.Member(t, W, "owner@corp.com")
	d.Page(t, W, owner, "Runbook")

	srv := mcp.New(page.NewStore(d.Pool), space.NewStore(d.Pool), nil, nil, nil, "test").
		WithAccess(mcpAccess(d))
	chain := chainForServer(d, srv)

	rr := callTool(chain, "owner@corp.com", true, "ask_docs",
		map[string]any{"workspace_id": W, "question": "what is the runbook?"})
	result, errObj := rpcEnvelope(t, rr.Body.Bytes())
	if errObj != nil {
		return // an error is the correct answer
	}
	text := contentText(t, result)
	var out struct {
		Answer  string `json:"answer"`
		Sources []any  `json:"sources"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode ask_docs payload: %v (%s)", err, text)
	}
	if strings.TrimSpace(out.Answer) == "" {
		// [ASK-NOT-EMPTY] the failure this guards: an unavailable service reported as an answer.
		t.Errorf("[ASK-NOT-EMPTY] ask_docs with NO AI engine returned a RESULT with an empty "+
			"answer and %d sources — an absent service must be an error, not a verdict on the "+
			"documents: %s", len(out.Sources), text)
	}
}
