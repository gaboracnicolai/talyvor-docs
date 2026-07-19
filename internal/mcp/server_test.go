package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/ai"
	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
)

// testWS is the workspace every fake object resolves to; rpc() injects a verified membership
// for it so tool-logic tests exercise the authorized path through the SEC-4 authz chokepoint.
const testWS = "ws-1"

// ─── Fakes ────────────────────────────────────────────────

type fakePages struct {
	byID    map[string]*model.Page
	search  []page.SearchResult
	list    []model.Page
	created *model.Page
	updated *model.Page
	stale   []model.Page
}

func (f *fakePages) GetByID(_ context.Context, id string) (*model.Page, error) {
	p := f.byID[id]
	if p != nil && p.WorkspaceID == "" {
		p.WorkspaceID = testWS // so the SEC-4 chokepoint resolves the page's workspace
	}
	return p, nil
}
func (f *fakePages) GetBySlug(_ context.Context, _, _ string) (*model.Page, error) {
	for _, p := range f.byID {
		if p.WorkspaceID == "" {
			p.WorkspaceID = testWS
		}
		return p, nil
	}
	return nil, nil
}
func (f *fakePages) SearchWithRank(_ context.Context, _, _ string, _ *string, _, _ int) ([]page.SearchResult, error) {
	return f.search, nil
}
func (f *fakePages) Create(_ context.Context, p model.Page) (*model.Page, error) {
	p.ID = "pg-new"
	f.created = &p
	return &p, nil
}
func (f *fakePages) Update(_ context.Context, id string, updates map[string]any) (*model.Page, error) {
	p := &model.Page{ID: id, UpdatedAt: time.Now().UTC()}
	if t, ok := updates["title"].(string); ok {
		p.Title = t
	}
	f.updated = p
	return p, nil
}
func (f *fakePages) List(_ context.Context, _ page.PageFilter) ([]model.Page, error) {
	return f.list, nil
}
func (f *fakePages) Verify(_ context.Context, _ /*pageID*/, _ /*verifierID*/ string) error {
	return nil
}
func (f *fakePages) GetStalePages(_ context.Context, _ string) ([]model.Page, error) {
	return f.stale, nil
}

type fakeSpaces struct {
	byID map[string]*model.Space
	list []model.Space
}

func (f *fakeSpaces) GetByID(_ context.Context, id string) (*model.Space, error) {
	if sp := f.byID[id]; sp != nil {
		if sp.WorkspaceID == "" {
			sp.WorkspaceID = testWS
		}
		return sp, nil
	}
	return &model.Space{ID: id, WorkspaceID: testWS}, nil // default: a space in the test workspace
}
func (f *fakeSpaces) List(_ context.Context, _ string) ([]model.Space, error) {
	return f.list, nil
}

type fakeAnalytics struct {
	stats *analytics.ReadStats
}

func (f *fakeAnalytics) GetReadStats(_ context.Context, _ string, _ int) (*analytics.ReadStats, error) {
	return f.stats, nil
}

type fakeAI struct {
	answer string
}

func (f *fakeAI) AskDocs(_ context.Context, _, _ string, _ []ai.PageContext) (string, error) {
	return f.answer, nil
}

// ─── Test helpers ─────────────────────────────────────────

// fakeAccess is a permissive tier gate for the unit tests, which exercise tool MECHANICS (markdown
// conversion, content shaping) with fakes and no permission store — not the tier itself (that is
// proven against real Postgres in sec_tier_test.go). Without a wired controller the write tools would
// fail closed and these mechanics tests couldn't run.
type fakeAccess struct{ allow bool }

func (f fakeAccess) CanEditPage(_ context.Context, _, _ string) (bool, error)  { return f.allow, nil }
func (f fakeAccess) CanEditSpace(_ context.Context, _, _ string) (bool, error) { return f.allow, nil }

func newTestServer(t *testing.T, pages pageDeps, spaces spaceDeps, analyticsDep analyticsDeps, aiDep aiDeps, freshDep freshDeps) *Server {
	t.Helper()
	return newServer(deps{
		pages:     pages,
		spaces:    spaces,
		analytics: analyticsDep,
		ai:        aiDep,
		freshness: freshDep,
		version:   "test",
	}).WithAccess(fakeAccess{allow: true})
}

func rpc(method string, params any, id int) *http.Request {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// SEC-4: tools now pass through the authz chokepoint, so exercise the authorized path —
	// a verified caller who is a member of testWS (mirrors the gateway+authz middleware).
	ctx := authz.WithMemberships(req.Context(), "agent@test", []authz.Membership{{WorkspaceID: testWS, MemberID: "m-agent"}})
	return req.WithContext(ctx)
}

func decodeResp(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not json: %v\n%s", err, rr.Body.String())
	}
	return out
}

// callTool invokes tools/call and returns the parsed `result` object.
func callTool(t *testing.T, srv *Server, tool string, args map[string]any) map[string]any {
	t.Helper()
	req := rpc("tools/call", map[string]any{"name": tool, "arguments": args}, 1)
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, req)
	resp := decodeResp(t, rr)
	if resp["error"] != nil {
		t.Fatalf("tools/call %s errored: %+v", tool, resp["error"])
	}
	result, _ := resp["result"].(map[string]any)
	return result
}

// ─── Tests ────────────────────────────────────────────────

func TestInitialize_ReturnsProtocolVersion(t *testing.T) {
	srv := newTestServer(t, &fakePages{}, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, rpc("initialize", map[string]any{}, 1))
	resp := decodeResp(t, rr)
	result, _ := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocol version = %v", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "talyvor-docs" {
		t.Fatalf("serverInfo.name = %v", info["name"])
	}
}

func TestToolsList_ReturnsAll10Tools(t *testing.T) {
	srv := newTestServer(t, &fakePages{}, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, rpc("tools/list", nil, 1))
	resp := decodeResp(t, rr)
	result, _ := resp["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 10 {
		t.Fatalf("want 10 tools, got %d", len(tools))
	}
	// Each tool must have name, description, inputSchema.
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] == "" || tool["description"] == "" {
			t.Fatalf("tool missing name/description: %+v", tool)
		}
		if tool["inputSchema"] == nil {
			t.Fatalf("tool %s missing inputSchema", tool["name"])
		}
	}
}

func TestSearchDocs_ReturnsResults(t *testing.T) {
	pages := &fakePages{
		search: []page.SearchResult{
			{
				Page:      model.Page{ID: "pg-1", Title: "Auth", SpaceID: "sp-1"},
				SpaceName: "Engineering",
				Rank:      0.9,
				Headline:  "Some <mark>auth</mark> excerpt",
			},
		},
	}
	srv := newTestServer(t, pages, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	res := callTool(t, srv, "search_docs", map[string]any{
		"query":        "auth",
		"workspace_id": "ws-1",
	})
	items := mustItems(t, res)
	if len(items) != 1 {
		t.Fatalf("want 1 search result, got %d", len(items))
	}
	if items[0].(map[string]any)["title"] != "Auth" {
		t.Fatalf("title not surfaced: %+v", items[0])
	}
}

func TestGetPage_ReturnsContentText_NotJSON(t *testing.T) {
	pages := &fakePages{
		byID: map[string]*model.Page{
			"pg-1": {
				ID:          "pg-1",
				SpaceID:     "sp-1",
				Title:       "Deploy",
				Slug:        "deploy",
				Content:     `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"run make"}]}]}`,
				ContentText: "run make",
				AICostUSD:   1.23,
			},
		},
	}
	srv := newTestServer(t, pages, &fakeSpaces{
		byID: map[string]*model.Space{"sp-1": {ID: "sp-1", Name: "Engineering"}},
	}, &fakeAnalytics{}, &fakeAI{}, nil)
	res := callTool(t, srv, "get_page", map[string]any{
		"page_id": "pg-1",
	})
	page := mustItem(t, res)
	if _, isString := page["content_text"].(string); !isString {
		t.Fatalf("content_text should be plain string, got %T", page["content_text"])
	}
	body := page["content_text"].(string)
	if strings.Contains(body, `"type":"doc"`) {
		t.Fatalf("content_text leaked raw ProseMirror JSON: %q", body)
	}
	if page["space_name"] != "Engineering" {
		t.Fatalf("space_name not surfaced: %v", page["space_name"])
	}
}

func TestCreatePage_AcceptsMarkdown(t *testing.T) {
	pages := &fakePages{}
	srv := newTestServer(t, pages, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	callTool(t, srv, "create_page", map[string]any{
		"space_id": "sp-1",
		"title":    "Test",
		"content":  "# Heading\n\nparagraph",
	})
	if pages.created == nil {
		t.Fatal("Create not called")
	}
	if !strings.Contains(pages.created.Content, `"type":"heading"`) {
		t.Fatalf("markdown not converted to ProseMirror: %s", pages.created.Content)
	}
}

func TestAskDocs_AnswersWithSources(t *testing.T) {
	pages := &fakePages{
		search: []page.SearchResult{
			{Page: model.Page{ID: "pg-1", Title: "Deploy guide", SpaceID: "sp-1"}, SpaceName: "Eng", Rank: 0.9, Headline: ""},
		},
	}
	srv := newTestServer(t, pages, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{answer: "Run make deploy."}, nil)
	res := callTool(t, srv, "ask_docs", map[string]any{
		"question":     "how to deploy?",
		"workspace_id": "ws-1",
	})
	item := mustItem(t, res)
	if !strings.Contains(item["answer"].(string), "make deploy") {
		t.Fatalf("answer wrong: %v", item["answer"])
	}
	sources, _ := item["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("want 1 source, got %d", len(sources))
	}
}

func TestGetStalePages_Returns(t *testing.T) {
	pages := &fakePages{
		stale: []model.Page{
			{ID: "pg-1", Title: "Old doc", SpaceID: "sp-1", StaleAfterDays: 30, UpdatedAt: time.Now().Add(-90 * 24 * time.Hour)},
		},
	}
	fresh := freshness.New(pages, nil, nil)
	srv := newTestServer(t, pages, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, fresh)
	res := callTool(t, srv, "get_stale_pages", map[string]any{
		"workspace_id": "ws-1",
	})
	items := mustItems(t, res)
	if len(items) != 1 {
		t.Fatalf("want 1 stale page, got %d", len(items))
	}
}

func TestGetSpaceTree_ReturnsNested(t *testing.T) {
	spaces := &fakeSpaces{
		list: []model.Space{{ID: "sp-1", Name: "Engineering"}},
	}
	pages := &fakePages{
		list: []model.Page{
			{ID: "pg-1", Title: "Index", SpaceID: "sp-1", Depth: 0},
			{ID: "pg-2", Title: "Child", SpaceID: "sp-1", Depth: 1},
		},
	}
	srv := newTestServer(t, pages, spaces, &fakeAnalytics{}, &fakeAI{}, nil)
	res := callTool(t, srv, "get_space_tree", map[string]any{
		"workspace_id": "ws-1",
	})
	items := mustItems(t, res)
	if len(items) != 1 {
		t.Fatalf("want 1 space, got %d", len(items))
	}
	space := items[0].(map[string]any)
	pagesList, _ := space["pages"].([]any)
	if len(pagesList) == 0 {
		t.Fatalf("space pages missing: %+v", space)
	}
}

func TestUnknownTool_DeniedFailClosed(t *testing.T) {
	srv := newTestServer(t, &fakePages{}, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, rpc("tools/call", map[string]any{"name": "no_such_tool"}, 1))
	resp := decodeResp(t, rr)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error, got %+v", resp)
	}
	// SEC-4: an unmapped tool is denied at the authz chokepoint (fail-closed, ws unresolved)
	// BEFORE the dispatcher's method-not-found — an unauthorized caller can't probe tool existence.
	if int(errObj["code"].(float64)) != -32001 {
		t.Fatalf("expected -32001 (unauthorized, fail-closed), got %v", errObj["code"])
	}
}

func TestMissingRequiredParam_ReturnsInvalidParams(t *testing.T) {
	srv := newTestServer(t, &fakePages{}, &fakeSpaces{}, &fakeAnalytics{}, &fakeAI{}, nil)
	// search_docs requires query + workspace_id. Supply an AUTHORIZED workspace_id (so the
	// chokepoint passes) but omit query → the tool's own param validation returns invalid-params.
	rr := httptest.NewRecorder()
	srv.HandleRPC(rr, rpc("tools/call", map[string]any{"name": "search_docs", "arguments": map[string]any{"workspace_id": testWS}}, 1))
	resp := decodeResp(t, rr)
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("expected error, got %+v", resp)
	}
	if int(errObj["code"].(float64)) != -32602 {
		t.Fatalf("expected -32602 (invalid params), got %v", errObj["code"])
	}
}

// mustItems pulls out the `content[0].text` JSON from an MCP tool
// response and parses it into a slice. MCP wraps each tool result
// as `{"content":[{"type":"text","text":"<json>"}]}` — the tests
// assert on the inner shape after re-parsing.
func mustItems(t *testing.T, result map[string]any) []any {
	t.Helper()
	raw := mustItemRaw(t, result)
	list, _ := raw.([]any)
	if list == nil {
		t.Fatalf("expected array result, got %T: %+v", raw, raw)
	}
	return list
}

func mustItem(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	raw := mustItemRaw(t, result)
	obj, _ := raw.(map[string]any)
	if obj == nil {
		t.Fatalf("expected object result, got %T: %+v", raw, raw)
	}
	return obj
}

func mustItemRaw(t *testing.T, result map[string]any) any {
	t.Helper()
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("result missing content: %+v", result)
	}
	first := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("tool result not JSON: %v\n%s", err, text)
	}
	return parsed
}
