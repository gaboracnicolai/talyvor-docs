package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/talyvor/docs/internal/ai"
	"github.com/talyvor/docs/internal/analytics"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/freshness"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/space"
)

const protocolVersion = "2024-11-05"

// pageDeps + spaceDeps + analyticsDeps + aiDeps are the narrow store
// shapes the MCP tools call into. Defined as interfaces so the
// server is unit-testable with in-memory fakes — the real *page.Store
// / *space.Store / etc. satisfy them structurally.
type pageDeps interface {
	GetByID(ctx context.Context, id string) (*model.Page, error)
	GetBySlug(ctx context.Context, spaceID, slug string) (*model.Page, error)
	SearchWithRank(ctx context.Context, workspaceID, query string, spaceID *string, limit, offset int) ([]page.SearchResult, error)
	Create(ctx context.Context, p model.Page) (*model.Page, error)
	Update(ctx context.Context, id string, updates map[string]any) (*model.Page, error)
	List(ctx context.Context, filter page.PageFilter) ([]model.Page, error)
	Verify(ctx context.Context, pageID, verifierID string) error
	GetStalePages(ctx context.Context, workspaceID string) ([]model.Page, error)
}

type spaceDeps interface {
	GetByID(ctx context.Context, id string) (*model.Space, error)
	List(ctx context.Context, workspaceID string) ([]model.Space, error)
}

type analyticsDeps interface {
	GetReadStats(ctx context.Context, pageID string, days int) (*analytics.ReadStats, error)
}

type aiDeps interface {
	AskDocs(ctx context.Context, workspaceID, question string, pages []ai.PageContext) (string, error)
}

type freshDeps interface {
	GetStaleReport(ctx context.Context, workspaceID string) ([]freshness.FreshnessReport, error)
}

type deps struct {
	pages     pageDeps
	spaces    spaceDeps
	analytics analyticsDeps
	ai        aiDeps
	freshness freshDeps
	version   string
}

type Server struct {
	deps deps
}

// New constructs the server with the real package stores. Tests use
// newServer with fakes.
func New(pages *page.Store, spaces *space.Store, analyticsStore *analytics.Store, aiEngine *ai.Engine, fresh *freshness.FreshnessEngine, version string) *Server {
	return newServer(deps{
		pages:     pages,
		spaces:    spaces,
		analytics: analyticsStore,
		ai:        aiEngine,
		freshness: fresh,
		version:   version,
	})
}

func newServer(d deps) *Server { return &Server{deps: d} }

// ─── JSON-RPC framing ────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// MCP / JSON-RPC error codes.
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
	errUnauthorized   = -32001 // SEC-4: not a member of / not authorized for the acted-on workspace
)

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ─── HTTP handlers ───────────────────────────────────────

// HandleRPC is the POST /mcp endpoint. Dispatches initialize,
// tools/list, and tools/call to the right handler. Anything else
// gets the standard method-not-found error.
func (s *Server) HandleRPC(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRPC(w, rpcResponse{Error: &rpcError{Code: errParse, Message: "parse error"}})
		return
	}
	switch req.Method {
	case "initialize":
		writeRPC(w, rpcResponse{ID: req.ID, Result: s.initialize()})
	case "tools/list":
		writeRPC(w, rpcResponse{ID: req.ID, Result: map[string]any{"tools": s.toolsList()}})
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeRPC(w, rpcResponse{ID: req.ID, Error: &rpcError{Code: errInvalidParams, Message: "invalid params"}})
			return
		}
		result, err := s.callTool(r.Context(), params.Name, params.Arguments)
		if err != nil {
			var rpcErr *rpcError
			if errors.As(err, &rpcErr) {
				writeRPC(w, rpcResponse{ID: req.ID, Error: rpcErr})
				return
			}
			writeRPC(w, rpcResponse{ID: req.ID, Error: &rpcError{Code: errInternal, Message: err.Error()}})
			return
		}
		writeRPC(w, rpcResponse{ID: req.ID, Result: result})
	default:
		writeRPC(w, rpcResponse{ID: req.ID, Error: &rpcError{Code: errMethodNotFound, Message: "method not found: " + req.Method}})
	}
}

// HandleSSE streams a keep-alive ping every 30s. MCP clients use
// this channel for server-pushed events; we have none today but the
// endpoint stays open so transport-aware clients don't reconnect.
func (s *Server) HandleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	// Initial event so clients know the stream opened cleanly.
	fmt.Fprintf(w, "event: ping\ndata: {\"ts\":%d}\n\n", time.Now().Unix())
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			fmt.Fprintf(w, "event: ping\ndata: {\"ts\":%d}\n\n", time.Now().Unix())
			flusher.Flush()
		}
	}
}

// ─── Methods ─────────────────────────────────────────────

func (s *Server) initialize() map[string]any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo": map[string]any{
			"name":    "talyvor-docs",
			"version": s.deps.version,
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	}
}

// toolSpec describes a registered tool to the client. The fields
// match the MCP JSON-RPC schema; we keep the slice in a stable
// order so the protocol message is reproducible.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (s *Server) toolsList() []toolSpec {
	return []toolSpec{
		{
			Name:        "search_docs",
			Description: "Search Talyvor Docs by query string. Returns the top ranked pages with title, space, excerpt, and URL. Use this first when the user asks any question about internal documentation.",
			InputSchema: schema(
				required("query", "workspace_id"),
				prop("query", "string", "Free-text search query"),
				prop("workspace_id", "string", "Workspace identifier"),
				prop("space_id", "string", "Optional space filter"),
				prop("limit", "integer", "Max results (default 5)"),
			),
		},
		{
			Name:        "get_page",
			Description: "Fetch a documentation page's content_text (plain text, not JSON) plus metadata. Pass either page_id, or slug + space_id. Use this after search_docs picks a candidate.",
			InputSchema: schema(
				prop("page_id", "string", "Page identifier"),
				prop("slug", "string", "Page slug (requires space_id)"),
				prop("space_id", "string", "Space identifier (required when using slug)"),
			),
		},
		{
			Name:        "create_page",
			Description: "Create a new documentation page. The content field accepts markdown — it's converted to ProseMirror server-side. Use parent_id to nest under another page.",
			InputSchema: schema(
				required("space_id", "title"),
				prop("space_id", "string", "Space to create the page in"),
				prop("title", "string", "Page title"),
				prop("content", "string", "Page body in markdown"),
				prop("parent_id", "string", "Optional parent page for nesting"),
				prop("created_by", "string", "Author identifier (default \"agent\")"),
			),
		},
		{
			Name:        "update_page",
			Description: "Update an existing page's title or content. The content field accepts markdown. Either field is optional; omit it to leave the value unchanged.",
			InputSchema: schema(
				required("page_id"),
				prop("page_id", "string", "Page identifier"),
				prop("title", "string", "New title"),
				prop("content", "string", "New body in markdown"),
				prop("updated_by", "string", "Author identifier (default \"agent\")"),
			),
		},
		{
			Name:        "list_pages",
			Description: "List pages in a space, optionally scoped to children of a specific parent_id. Returns id, title, depth, last update time, and view_count.",
			InputSchema: schema(
				required("space_id"),
				prop("space_id", "string", "Space identifier"),
				prop("parent_id", "string", "Restrict to children of this page"),
				prop("limit", "integer", "Max results (default 20)"),
			),
		},
		{
			Name:        "ask_docs",
			Description: "Ask a natural-language question about the documentation. Returns a synthesised answer plus citations to the source pages. This is the highest-quality lookup — prefer it over search_docs when the user wants an answer, not a list.",
			InputSchema: schema(
				required("question", "workspace_id"),
				prop("question", "string", "Natural-language question"),
				prop("workspace_id", "string", "Workspace identifier"),
			),
		},
		{
			Name:        "get_stale_pages",
			Description: "List pages flagged as stale by the freshness engine — past their stale_after_days threshold or with linked Track issues completed since the last edit. Use this to find docs that need updating.",
			InputSchema: schema(
				required("workspace_id"),
				prop("workspace_id", "string", "Workspace identifier"),
			),
		},
		{
			Name:        "verify_page",
			Description: "Mark a page as verified (still accurate). This stamps last_verified_at and pulls the page off the stale list.",
			InputSchema: schema(
				required("page_id"),
				prop("page_id", "string", "Page identifier"),
				prop("verified_by", "string", "Verifier identifier (default \"agent\")"),
			),
		},
		{
			Name:        "get_page_analytics",
			Description: "Get readership stats for a page over the last N days (default 7). Returns total views, unique viewers, average dwell time, and last view timestamp.",
			InputSchema: schema(
				required("page_id"),
				prop("page_id", "string", "Page identifier"),
				prop("days", "integer", "Time window in days (default 7)"),
			),
		},
		{
			Name:        "get_space_tree",
			Description: "Return a map of the documentation: every space in the workspace with a 2-level nested page list. Use this when an agent needs to understand the shape of the knowledge base before drilling in.",
			InputSchema: schema(
				required("workspace_id"),
				prop("workspace_id", "string", "Workspace identifier"),
				prop("space_id", "string", "Restrict to a single space"),
			),
		},
	}
}

// schema is the JSONSchema builder for tool inputSchema fields. We
// keep it terse because every tool needs the same shape with just
// the properties + required list varying.
func schema(parts ...func(map[string]any)) map[string]any {
	s := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	for _, p := range parts {
		p(s)
	}
	return s
}
func prop(name, typ, desc string) func(map[string]any) {
	return func(s map[string]any) {
		props, _ := s["properties"].(map[string]any)
		props[name] = map[string]any{"type": typ, "description": desc}
	}
}
func required(names ...string) func(map[string]any) {
	return func(s map[string]any) {
		anys := make([]any, len(names))
		for i, n := range names {
			anys[i] = n
		}
		s["required"] = anys
	}
}

// ─── Tool dispatch ───────────────────────────────────────

func (s *Server) callTool(ctx context.Context, name string, args map[string]any) (any, error) {
	// SEC-4 authz chokepoint (mirrors Track's handleToolsCall): resolve the acted-on workspace —
	// from the workspace_id arg, or from the object the tool touches — and authorize it against
	// the VERIFIED caller's memberships BEFORE dispatch. Fail-closed by construction: an unmapped
	// tool, a missing/nonexistent object, or a non-member all yield deny — a tool never dispatches
	// on an unauthorized workspace, and a new tool cannot be an open surface.
	ws, err := s.toolWorkspace(ctx, name, args)
	if err != nil || ws == "" {
		return nil, &rpcError{Code: errUnauthorized, Message: "not authorized for the requested workspace"}
	}
	m, ok := authz.AuthorizeWorkspace(ctx, ws)
	if !ok {
		return nil, &rpcError{Code: errUnauthorized, Message: "not a member of this workspace"}
	}
	ctx = authz.WithAuthorized(ctx, m.WorkspaceID, m.MemberID)

	switch name {
	case "search_docs":
		return s.toolSearchDocs(ctx, args)
	case "get_page":
		return s.toolGetPage(ctx, args)
	case "create_page":
		return s.toolCreatePage(ctx, args)
	case "update_page":
		return s.toolUpdatePage(ctx, args)
	case "list_pages":
		return s.toolListPages(ctx, args)
	case "ask_docs":
		return s.toolAskDocs(ctx, args)
	case "get_stale_pages":
		return s.toolGetStalePages(ctx, args)
	case "verify_page":
		return s.toolVerifyPage(ctx, args)
	case "get_page_analytics":
		return s.toolGetPageAnalytics(ctx, args)
	case "get_space_tree":
		return s.toolGetSpaceTree(ctx, args)
	}
	return nil, &rpcError{Code: errMethodNotFound, Message: "unknown tool: " + name}
}

// toolWorkspace resolves the workspace a tool call acts on, for the authz chokepoint. Direct
// (the workspace_id arg) for workspace-keyed tools; resolved from the touched object's workspace
// for page/space-keyed tools. Returns "" for an unmapped tool or an unresolvable object so the
// chokepoint denies (fail-closed).
func (s *Server) toolWorkspace(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "search_docs", "ask_docs", "get_stale_pages", "get_space_tree":
		return stringArg(args, "workspace_id", ""), nil
	case "create_page", "list_pages":
		return s.spaceWorkspace(ctx, stringArg(args, "space_id", ""))
	case "update_page", "verify_page", "get_page_analytics":
		return s.pageWorkspace(ctx, stringArg(args, "page_id", ""))
	case "get_page":
		if pid := stringArg(args, "page_id", ""); pid != "" {
			return s.pageWorkspace(ctx, pid)
		}
		return s.spaceWorkspace(ctx, stringArg(args, "space_id", ""))
	}
	return "", nil // unmapped tool → deny
}

// pageWorkspace returns the workspace a page belongs to (to authorize a page-keyed tool). A
// missing/nonexistent page (or a lookup error) yields "" → deny — never a full-table leak.
func (s *Server) pageWorkspace(ctx context.Context, pageID string) (string, error) {
	if pageID == "" {
		return "", nil
	}
	p, err := s.deps.pages.GetByID(ctx, pageID)
	if err != nil || p == nil {
		return "", nil
	}
	return p.WorkspaceID, nil
}

// spaceWorkspace returns the workspace a space belongs to (to authorize a space-keyed tool).
func (s *Server) spaceWorkspace(ctx context.Context, spaceID string) (string, error) {
	if spaceID == "" {
		return "", nil
	}
	sp, err := s.deps.spaces.GetByID(ctx, spaceID)
	if err != nil || sp == nil {
		return "", nil
	}
	return sp.WorkspaceID, nil
}

// requireStrings ensures every argument named in `names` is present
// and non-empty, returning an invalid-params error otherwise.
func requireStrings(args map[string]any, names ...string) error {
	for _, n := range names {
		v, _ := args[n].(string)
		if strings.TrimSpace(v) == "" {
			return &rpcError{Code: errInvalidParams, Message: "missing required argument: " + n}
		}
	}
	return nil
}

func stringArg(args map[string]any, name, def string) string {
	if v, ok := args[name].(string); ok && v != "" {
		return v
	}
	return def
}

func intArg(args map[string]any, name string, def int) int {
	if v, ok := args[name].(float64); ok {
		return int(v)
	}
	return def
}

// toolContent wraps a JSON-encoded payload in the standard MCP
// content envelope. Every tool returns the same shape; the agent
// parses content[0].text back into structured data.
func toolContent(payload any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(body)},
		},
	}, nil
}

// ─── Individual tools ────────────────────────────────────

type searchHit struct {
	PageID    string  `json:"page_id"`
	Title     string  `json:"title"`
	SpaceName string  `json:"space_name"`
	Excerpt   string  `json:"excerpt"`
	URL       string  `json:"url"`
	Rank      float64 `json:"rank,omitempty"`
}

func (s *Server) toolSearchDocs(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "query", "workspace_id"); err != nil {
		return nil, err
	}
	q := stringArg(args, "query", "")
	wsID := stringArg(args, "workspace_id", "")
	var spaceID *string
	if v := stringArg(args, "space_id", ""); v != "" {
		spaceID = &v
	}
	limit := intArg(args, "limit", 5)
	results, err := s.deps.pages.SearchWithRank(ctx, wsID, q, spaceID, limit, 0)
	if err != nil {
		return nil, err
	}
	hits := make([]searchHit, 0, len(results))
	for _, r := range results {
		hits = append(hits, searchHit{
			PageID:    r.Page.ID,
			Title:     r.Page.Title,
			SpaceName: r.SpaceName,
			Excerpt:   stripMarks(r.Headline),
			URL:       pageURL(r.Page.SpaceID, r.Page.ID),
			Rank:      r.Rank,
		})
	}
	return toolContent(hits)
}

type pageOut struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	ContentText     string  `json:"content_text"`
	SpaceName       string  `json:"space_name"`
	URL             string  `json:"url"`
	FreshnessStatus string  `json:"freshness_status"`
	AICostUSD       float64 `json:"ai_cost_usd"`
	LastUpdated     string  `json:"last_updated"`
	VerifiedBy      string  `json:"verified_by,omitempty"`
}

func (s *Server) toolGetPage(ctx context.Context, args map[string]any) (any, error) {
	pageID := stringArg(args, "page_id", "")
	slug := stringArg(args, "slug", "")
	spaceID := stringArg(args, "space_id", "")
	if pageID == "" && slug == "" {
		return nil, &rpcError{Code: errInvalidParams, Message: "page_id or slug required"}
	}
	var p *model.Page
	var err error
	if pageID != "" {
		p, err = s.deps.pages.GetByID(ctx, pageID)
	} else {
		if spaceID == "" {
			return nil, &rpcError{Code: errInvalidParams, Message: "space_id required when using slug"}
		}
		p, err = s.deps.pages.GetBySlug(ctx, spaceID, slug)
	}
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, &rpcError{Code: errInvalidParams, Message: "page not found"}
	}
	spaceName := ""
	if s.deps.spaces != nil {
		if sp, _ := s.deps.spaces.GetByID(ctx, p.SpaceID); sp != nil {
			spaceName = sp.Name
		}
	}
	// content_text is what we promised in the spec — plain prose,
	// readable by an LLM. We prefer the dehydrated column when set;
	// otherwise we convert the ProseMirror JSON on the fly.
	body := p.ContentText
	if body == "" {
		body = ProseMirrorToMarkdown(p.Content)
	}
	verifiedBy := ""
	if p.VerifiedBy != nil {
		verifiedBy = *p.VerifiedBy
	}
	out := pageOut{
		ID:              p.ID,
		Title:           p.Title,
		ContentText:     body,
		SpaceName:       spaceName,
		URL:             pageURL(p.SpaceID, p.ID),
		FreshnessStatus: deriveFreshness(p),
		AICostUSD:       p.AICostUSD,
		LastUpdated:     p.UpdatedAt.UTC().Format(time.RFC3339),
		VerifiedBy:      verifiedBy,
	}
	return toolContent(out)
}

type createOut struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

func (s *Server) toolCreatePage(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "space_id", "title"); err != nil {
		return nil, err
	}
	md := stringArg(args, "content", "")
	pm := MarkdownToProseMirror(md)
	created, err := s.deps.pages.Create(ctx, model.Page{
		SpaceID:   stringArg(args, "space_id", ""),
		Title:     stringArg(args, "title", ""),
		Content:   pm,
		CreatedBy: stringArg(args, "created_by", "agent"),
	})
	if err != nil {
		return nil, err
	}
	return toolContent(createOut{ID: created.ID, Title: created.Title, URL: pageURL(created.SpaceID, created.ID)})
}

type updateOut struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Server) toolUpdatePage(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "page_id"); err != nil {
		return nil, err
	}
	updates := map[string]any{}
	if v := stringArg(args, "title", ""); v != "" {
		updates["title"] = v
	}
	if v := stringArg(args, "content", ""); v != "" {
		updates["content"] = MarkdownToProseMirror(v)
	}
	if v := stringArg(args, "updated_by", ""); v != "" {
		updates["updated_by"] = v
	} else {
		updates["updated_by"] = "agent"
	}
	p, err := s.deps.pages.Update(ctx, stringArg(args, "page_id", ""), updates)
	if err != nil {
		return nil, err
	}
	return toolContent(updateOut{ID: p.ID, Title: p.Title, UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339)})
}

type listOut struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Depth     int    `json:"depth"`
	UpdatedAt string `json:"updated_at"`
	ViewCount int    `json:"view_count"`
}

func (s *Server) toolListPages(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "space_id"); err != nil {
		return nil, err
	}
	filter := page.PageFilter{
		SpaceID: stringArg(args, "space_id", ""),
		Limit:   intArg(args, "limit", 20),
	}
	if v := stringArg(args, "parent_id", ""); v != "" {
		filter.ParentID = &v
	}
	pages, err := s.deps.pages.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	out := make([]listOut, 0, len(pages))
	for _, p := range pages {
		out = append(out, listOut{
			ID:        p.ID,
			Title:     p.Title,
			Depth:     p.Depth,
			UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
			ViewCount: p.ViewCount,
		})
	}
	return toolContent(out)
}

type askOut struct {
	Answer  string      `json:"answer"`
	Sources []askSource `json:"sources"`
}

type askSource struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	PageID string `json:"page_id"`
}

func (s *Server) toolAskDocs(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "question", "workspace_id"); err != nil {
		return nil, err
	}
	question := stringArg(args, "question", "")
	wsID := stringArg(args, "workspace_id", "")
	// Gather top-3 context pages via the full-text rank query — the
	// same approach the REST /ask endpoint uses.
	hits, _ := s.deps.pages.SearchWithRank(ctx, wsID, question, nil, 3, 0)
	pages := make([]ai.PageContext, 0, len(hits))
	sources := make([]askSource, 0, len(hits))
	for _, h := range hits {
		url := pageURL(h.Page.SpaceID, h.Page.ID)
		pages = append(pages, ai.PageContext{
			Title:   h.Page.Title,
			Content: h.Page.ContentText,
			URL:     url,
		})
		sources = append(sources, askSource{Title: h.Page.Title, URL: url, PageID: h.Page.ID})
	}
	answer := ""
	if s.deps.ai != nil {
		ans, err := s.deps.ai.AskDocs(ctx, wsID, question, pages)
		if err == nil {
			answer = ans
		}
	}
	return toolContent(askOut{Answer: answer, Sources: sources})
}

type stalePageOut struct {
	PageID        string `json:"page_id"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	DaysSinceEdit int    `json:"days_since_edit"`
	Reason        string `json:"reason,omitempty"`
	SpaceID       string `json:"space_id"`
}

func (s *Server) toolGetStalePages(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "workspace_id"); err != nil {
		return nil, err
	}
	if s.deps.freshness == nil {
		return toolContent([]stalePageOut{})
	}
	reports, err := s.deps.freshness.GetStaleReport(ctx, stringArg(args, "workspace_id", ""))
	if err != nil {
		return nil, err
	}
	out := make([]stalePageOut, 0, len(reports))
	for _, r := range reports {
		out = append(out, stalePageOut{
			PageID:        r.PageID,
			Title:         r.Title,
			Status:        string(r.Status),
			DaysSinceEdit: r.DaysSinceEdit,
			Reason:        r.Reason,
			SpaceID:       r.SpaceID,
		})
	}
	return toolContent(out)
}

func (s *Server) toolVerifyPage(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "page_id"); err != nil {
		return nil, err
	}
	if err := s.deps.pages.Verify(ctx, stringArg(args, "page_id", ""), stringArg(args, "verified_by", "agent")); err != nil {
		return nil, err
	}
	return toolContent(map[string]any{
		"ok":          true,
		"verified_at": time.Now().UTC().Format(time.RFC3339),
	})
}

type pageAnalyticsOut struct {
	TotalViews     int    `json:"total_views"`
	UniqueViewers  int    `json:"unique_viewers"`
	AvgDurationSec int    `json:"avg_duration_sec"`
	LastViewedAt   string `json:"last_viewed_at,omitempty"`
}

func (s *Server) toolGetPageAnalytics(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "page_id"); err != nil {
		return nil, err
	}
	days := intArg(args, "days", 7)
	stats, err := s.deps.analytics.GetReadStats(ctx, stringArg(args, "page_id", ""), days)
	if err != nil {
		return nil, err
	}
	out := pageAnalyticsOut{}
	if stats != nil {
		out.TotalViews = stats.TotalViews
		out.UniqueViewers = stats.UniqueViewers
		out.AvgDurationSec = stats.AvgDurationSec
		if stats.LastViewedAt != nil {
			out.LastViewedAt = stats.LastViewedAt.UTC().Format(time.RFC3339)
		}
	}
	return toolContent(out)
}

type spaceTreeOut struct {
	SpaceID string          `json:"space_id"`
	Name    string          `json:"name"`
	Icon    string          `json:"icon"`
	Pages   []spaceTreePage `json:"pages"`
}

type spaceTreePage struct {
	PageID   string          `json:"page_id"`
	Title    string          `json:"title"`
	Children []spaceTreePage `json:"children,omitempty"`
}

func (s *Server) toolGetSpaceTree(ctx context.Context, args map[string]any) (any, error) {
	if err := requireStrings(args, "workspace_id"); err != nil {
		return nil, err
	}
	wsID := stringArg(args, "workspace_id", "")
	scopeSpaceID := stringArg(args, "space_id", "")

	spaces, err := s.deps.spaces.List(ctx, wsID)
	if err != nil {
		return nil, err
	}
	out := make([]spaceTreeOut, 0, len(spaces))
	for _, sp := range spaces {
		if scopeSpaceID != "" && sp.ID != scopeSpaceID {
			continue
		}
		pages, _ := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
		// Bucket pages by parent for a 2-level nested view.
		byParent := map[string][]model.Page{}
		for _, p := range pages {
			parent := ""
			if p.ParentID != nil {
				parent = *p.ParentID
			}
			byParent[parent] = append(byParent[parent], p)
		}
		roots := byParent[""]
		sort.SliceStable(roots, func(i, j int) bool { return roots[i].Position < roots[j].Position })
		var entries []spaceTreePage
		for _, root := range roots {
			children := byParent[root.ID]
			sort.SliceStable(children, func(i, j int) bool { return children[i].Position < children[j].Position })
			kids := make([]spaceTreePage, 0, len(children))
			for _, c := range children {
				kids = append(kids, spaceTreePage{PageID: c.ID, Title: c.Title})
			}
			entries = append(entries, spaceTreePage{PageID: root.ID, Title: root.Title, Children: kids})
		}
		out = append(out, spaceTreeOut{
			SpaceID: sp.ID,
			Name:    sp.Name,
			Icon:    sp.Icon,
			Pages:   entries,
		})
	}
	return toolContent(out)
}

// ─── Helpers ─────────────────────────────────────────────

func pageURL(spaceID, pageID string) string {
	if spaceID == "" {
		return "/pages/" + pageID
	}
	return "/spaces/" + spaceID + "/pages/" + pageID
}

// stripMarks removes the <mark> highlights that the search headline
// uses. Agents don't need the highlights, and they'd confuse an LLM
// trying to read the excerpt verbatim.
func stripMarks(s string) string {
	s = strings.ReplaceAll(s, "<mark>", "")
	s = strings.ReplaceAll(s, "</mark>", "")
	return s
}

// deriveFreshness is a cheap status derivation that doesn't require
// the full freshness engine. It mirrors the engine's classification
// rules so the get_page tool stays self-contained.
func deriveFreshness(p *model.Page) string {
	if p.StaleAfterDays <= 0 {
		return "unknown"
	}
	now := time.Now().UTC()
	effective := p.UpdatedAt
	if p.LastVerifiedAt != nil && p.LastVerifiedAt.After(effective) {
		effective = *p.LastVerifiedAt
	}
	days := int(now.Sub(effective) / (24 * time.Hour))
	if days >= p.StaleAfterDays {
		return "stale"
	}
	if float64(days) >= float64(p.StaleAfterDays)*0.5 {
		return "warning"
	}
	return "fresh"
}

// errRPC is a convenience so tool implementations can return typed
// JSON-RPC errors without importing the (private) rpcError struct
// directly.
func errRPC(code int, msg string) error {
	return &rpcError{Code: code, Message: msg}
}

// Error makes rpcError satisfy the error interface.
func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc %d: %s", e.Code, e.Message)
}
