package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/talyvor/docs/internal/ratelimit"
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
	deps   deps
	limit  *ratelimit.Limiter // nil = unthrottled (tests); main.go always wires it
	access AccessController   // nil = write tools DENY (fail-closed); main.go always wires it
}

// WithRateLimit attaches the per-workspace LLM limiter. It applies ONLY to llmTools, keyed
// on the workspace callTool has already VERIFIED — never the client-supplied workspace_id
// arg, which would let an agent name a fresh bucket and evade the ceiling.
func (s *Server) WithRateLimit(l *ratelimit.Limiter) *Server {
	s.limit = l
	return s
}

// WithAccess attaches the within-workspace tier gate for the write tools (create/update/verify_page).
// The membership chokepoint authorizes the workspace; this enforces the AccessEdit tier inside it, so
// a view-tier member cannot mutate via MCP as they cannot via REST. Unwired ⇒ write tools DENY.
func (s *Server) WithAccess(a AccessController) *Server {
	s.access = a
	return s
}

// New constructs the server with the real package stores. Tests use
// newServer with fakes.
//
// ⚠ EVERY ARGUMENT IS A CONCRETE POINTER AND EVERY deps FIELD IS AN INTERFACE, so assigning one
// straight through turns a nil pointer into a NON-NIL interface holding nil. That is not a
// nuisance, it is the thing that disarms the nil guards downstream: `s.deps.spaces != nil` is
// TRUE for a nil *space.Store, and space.Store.GetByID dereferences s.pool with no receiver check
// — measured as a get_page panic in typednil_deps_realpg_test.go. freshness.GetStaleReport
// records the same footgun as a SIGSEGV it answered with a nil-RECEIVER check, and hands this
// half on by name ("THE DEAD nil GUARD IN internal/mcp IS ITS OWN FINDING").
//
// So the conversion happens HERE, once, and every guard downstream asks a question that can
// actually be false. A dep added later inherits it by writing one more line in this block, which
// is the cheapest place in the package to remember.
//
// ⚠ AND MAKING THEM LIVE IS NOT FREE — two fallbacks these guards were hiding are LIES, and both
// move in this same commit rather than being switched on underneath: get_stale_pages answered an
// absent engine with `[]` ("nothing needs attention") and ask_docs with `answer: ""` ("the
// documents say nothing"). Both are errors now. Turning a dead guard live without fixing what it
// guards would have made this class worse, not better.
//
// ⚠ deps.analytics WAS THE ONE THIS BLOCK COULD NOT HELP — it had NO guard at any call site, so
// get_page_analytics with a nil *analytics.Store panicked both before and after this
// normalisation. THAT IS NOW FIXED AT ITS CALL SITE (toolGetPageAnalytics), which is where the
// guard had to go: the recommendation left here — a nil-RECEIVER check on analytics.Store — was
// MEASURED INERT, because this block makes the interface itself nil and the call panics at
// dispatch without ever entering the method. Read the note at toolGetPageAnalytics before
// answering a "just add a receiver check" review comment on any other dep.
func New(pages *page.Store, spaces *space.Store, analyticsStore *analytics.Store, aiEngine *ai.Engine, fresh *freshness.FreshnessEngine, version string) *Server {
	d := deps{version: version}
	if pages != nil {
		d.pages = pages
	}
	if spaces != nil {
		d.spaces = spaces
	}
	if analyticsStore != nil {
		d.analytics = analyticsStore
	}
	if aiEngine != nil {
		d.ai = aiEngine
	}
	if fresh != nil {
		d.freshness = fresh
	}
	return newServer(d)
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
	errRateLimited    = -32002 // this workspace has exceeded its LLM rate ceiling
)

// llmTools are the tools that reach Lens and therefore spend. Only these are rate-limited:
// /mcp is ONE JSON-RPC door for 10 tools, and throttling get_page because someone used
// ask_docs would be wrong. Keep this set honest — a new tool that calls the AI engine
// belongs here, or it is an unmetered hole.
var llmTools = map[string]bool{
	"ask_docs": true,
}

// writeTools are the tools that MUTATE a resource and therefore require the AccessEdit tier (not just
// workspace membership). Keep this set honest — a new mutating tool that is not listed here bypasses the
// tier gate, the same class of hole this closes for create/update/verify_page.
var writeTools = map[string]bool{
	"create_page": true,
	"update_page": true,
	"verify_page": true,
}

// gatedReadTools are the READ tools whose object is named by a client arg — one page, one space,
// chosen by the caller. There is no honest partial answer to "give me THIS object", so these DENY
// at the VIEW tier, which is what the REST by-id doors do via Enforcer.Require(AccessView).
//
// ⚠ THE READ TOOLS THAT ARE NOT HERE ARE NOT UNGATED — THEY ARE GATED IN A DIFFERENT SHAPE,
// AND CONFUSING THE TWO IS HOW THIS SET GOES STALE:
//
//   - search_docs and get_space_tree return a MIXED set the caller did not enumerate. Denying the
//     call would take the readable rows away with the unreadable ones, so they FILTER per row
//     inside the tool (over an over-fetched window, so a hidden row cannot eat a result slot).
//   - get_page gates INSIDE the tool because its object does not exist until the lookup has run:
//     the slug arm carries only (space_id, slug), so a gate here would have to resolve the same
//     row a second time, on the other side of the decision it is making.
//   - get_stale_pages is filtered one layer FURTHER OUT, by freshness.GetStaleReport's
//     AuthorizePageRead pass, and ask_docs filters its GROUNDING CORPUS per page before the rows
//     reach the model.
//
// ⚠ THIS SENTENCE USED TO SAY "THE THREE READ TOOLS THAT ARE NOT HERE" AND NAME THE FIRST THREE.
// Measured against the dispatch switch there are FOUR reads outside this set, five counting
// ask_docs — the count was written when it was true and nothing could see it stop being true. The
// full list, with the reason for each, is now machine-checked against the dispatch switch on every
// run by accessExemptTools in toolclassification_test.go, so a prose count cannot drift again.
//
// authorizeRead's switch DENIES an unmapped member of this set, so adding a name here without a
// rule is a loud total denial rather than a silent hole.
var gatedReadTools = map[string]bool{
	"list_pages":         true,
	"get_page_analytics": true,
}

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
			Name: "get_stale_pages",
			// ⚠ THE SECOND CRITERION THIS USED TO PROMISE WAS UNSATISFIABLE BY CONSTRUCTION,
			// AND AN AGENT CANNOT TELL AN EMPTY LIST FROM AN ABSENT ONE. It read "past their
			// stale_after_days threshold OR with linked Track issues completed since the last
			// edit". The whole population is page.Store.GetStalePages, one SQL predicate over
			// stale_after_days — the linked-issue signal is computed only for pages that
			// predicate ALREADY returned, so it can annotate a row and can never add one.
			// MEASURED on real Postgres with the links and the Track client genuinely wired
			// (internal/freshness/stalereport_population_realpg_test.go): a page with
			// stale_after_days = 0 and BOTH linked issues `done` is absent here while
			// get_page reports suggest_review = true for that same page. The boundary is
			// stated rather than implied, and get_page is named, because an agent that reads
			// "or" and gets [] concludes no such document exists.
			Description: "List pages past their stale_after_days threshold. Use this to find docs that need updating. LIMITS, so an empty list is not misread: a page with no stale_after_days set never appears here, and completed linked Track issues never add a page to this list — they only annotate one already past its threshold. For a single page's full freshness signal, including linked-issue activity, call get_page.",
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

	// LLM spend ceiling, per VERIFIED workspace (m.WorkspaceID — never the workspace_id
	// arg). Only the tools that actually reach Lens; an agent loop can call ask_docs far
	// faster than a human clicks, on Docs's single unmetered service key.
	if llmTools[name] && s.limit != nil && !s.limit.Allow(m.WorkspaceID) {
		return nil, &rpcError{Code: errRateLimited, Message: "rate limit exceeded for this workspace, please retry shortly"}
	}

	// SEC-4 within-workspace TIER gate. The chokepoint above authorized the WORKSPACE; the write
	// tools additionally require the AccessEdit tier on the target page/space — the same the REST
	// doors enforce — so a view-tier member cannot create/update/verify via MCP. Fail-closed.
	if writeTools[name] {
		allowed, err := s.authorizeWrite(ctx, name, args, m.MemberID)
		if err != nil || !allowed {
			return nil, &rpcError{Code: errUnauthorized, Message: "not authorized: this action requires edit access"}
		}
	}

	// SEC-4 within-workspace VIEW gate, the read half of the same idea. The chokepoint above
	// authorized the WORKSPACE; a member with no grant on a private space is a member of that
	// workspace, so membership alone let every read tool through. Fail-closed.
	if gatedReadTools[name] {
		allowed, err := s.authorizeRead(ctx, name, args, m.MemberID)
		if err != nil || !allowed {
			return nil, &rpcError{Code: errUnauthorized, Message: "not authorized: this action requires view access"}
		}
	}

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

// authorizeWrite enforces the AccessEdit tier a write tool needs, on the object it targets. It reuses
// the permission rule engine via the AccessController (permission.CheckPage / CheckSpace) — the same
// resolveAccess the REST enforcer runs. FAIL-CLOSED: a nil controller denies every write, so a dropped
// wiring is a loud total denial rather than a silent reopening of the tier hole. memberID is the actor
// the chokepoint already resolved from the VERIFIED identity (never a client arg).
func (s *Server) authorizeWrite(ctx context.Context, name string, args map[string]any, memberID string) (bool, error) {
	if s.access == nil {
		return false, nil
	}
	switch name {
	case "create_page":
		return s.access.CanEditSpace(ctx, stringArg(args, "space_id", ""), memberID)
	case "update_page", "verify_page":
		return s.access.CanEditPage(ctx, stringArg(args, "page_id", ""), memberID)
	}
	return false, nil // an unmapped write tool denies (fail-closed)
}

// authorizeRead enforces the AccessView tier a gatedReadTools entry needs, on the object its args
// name. Same engine, same lookers, same fail-closed nil as authorizeWrite — the only difference is
// the tier, because reading is not editing.
func (s *Server) authorizeRead(ctx context.Context, name string, args map[string]any, memberID string) (bool, error) {
	if s.access == nil {
		return false, nil
	}
	switch name {
	case "list_pages":
		return s.access.CanViewSpace(ctx, stringArg(args, "space_id", ""), memberID)
	case "get_page_analytics":
		return s.access.CanViewPage(ctx, stringArg(args, "page_id", ""), memberID)
	}
	return false, nil // an unmapped read tool denies (fail-closed)
}

// canViewPage / canViewSpace are the in-tool form of the same question, for the three tools that
// cannot be answered by a yes/no at dispatch. They read the actor the chokepoint already resolved
// from the VERIFIED identity (never a client arg) and swallow the error into a DENIAL: a tier that
// could not be resolved is not a tier that was granted.
func (s *Server) canViewPage(ctx context.Context, pageID string) bool {
	if s.access == nil || pageID == "" {
		return false
	}
	actor, ok := authz.AuthorizedMember(ctx)
	if !ok || actor == "" {
		return false
	}
	allowed, err := s.access.CanViewPage(ctx, pageID, actor)
	return err == nil && allowed
}

func (s *Server) canViewSpace(ctx context.Context, spaceID string) bool {
	if s.access == nil || spaceID == "" {
		return false
	}
	actor, ok := authz.AuthorizedMember(ctx)
	if !ok || actor == "" {
		return false
	}
	allowed, err := s.access.CanViewSpace(ctx, spaceID, actor)
	return err == nil && allowed
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
	// AN UNWIRED STORE IS THE MOST UNRESOLVABLE AN OBJECT GETS, SO IT TAKES THE RULE THIS
	// FUNCTION ALREADY STATES: "" → deny. #141 made mcp.New leave these interfaces genuinely nil
	// and armed the one dead guard it found (get_page's space-name enrichment); the two
	// chokepoint resolvers deref their store BARE and were never covered, so a nil store
	// panicked INSIDE the authorization step, before any tool body ran.
	//
	// ⚠ THE COST, STATED RATHER THAN HIDDEN: a misconfigured server now answers 'forbidden'
	// rather than 'misconfigured' on the tools routed through here. That is the direction this
	// chokepoint already chose for every other unresolvable case, and the alternative reports
	// server configuration state down an authorization path.
	if s.deps.pages == nil || pageID == "" {
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
	// Same rule as pageWorkspace above, and read its note for why deny rather than error.
	//
	// ⚠ THIS ONE ALSO COVERS get_page's SECOND ARM, WHICH IS WHY #141's GREEN TEST DID NOT CLOSE
	// THE CLASS: toolWorkspace sends get_page here whenever it is called with space_id instead of
	// page_id, so the tool typednil_deps_realpg_test.go declares safe with a nil *space.Store had
	// an unguarded path into that same store all along.
	if s.deps.spaces == nil || spaceID == "" {
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
	// OVER-FETCH, BECAUSE THE ROWS THE CALLER MAY NOT READ ARE DROPPED AFTER THE SQL LIMIT.
	// Measured on the REST twin (#78): with limit=1 and one hidden page ranking above a readable
	// one, the store returns the hidden row alone, the filter drops it, and the caller is told
	// NOTHING MATCHED for a document they may open. It is a mitigation, not a guarantee — a run of
	// more than (searchFetchFactor-1)×limit consecutive hidden rows still under-fills — and this
	// payload has always been a bare list with no total, so nothing here newly misreports a count.
	fetchLimit := limit
	if s.access != nil {
		fetchLimit = limit * searchFetchFactor
		if fetchLimit > searchMaxFetchRows {
			fetchLimit = searchMaxFetchRows
		}
	}
	results, err := s.deps.pages.SearchWithRank(ctx, wsID, q, spaceID, fetchLimit, 0)
	if err != nil {
		return nil, err
	}
	hits := make([]searchHit, 0, len(results))
	for _, r := range results {
		// The VIEW tier, per row, BEFORE truncation. A workspace-scoped search is the one shape
		// where denying the whole call would be wrong: the readable rows are the answer.
		if !s.canViewPage(ctx, r.Page.ID) {
			continue
		}
		if limit > 0 && len(hits) >= limit {
			break
		}
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

// searchFetchFactor / searchMaxFetchRows size the window SearchWithRank is asked for when the access
// gate is wired. searchMaxFetchRows is the STORE's own ceiling (SearchWithRank clamps limit to 50),
// named here so the two numbers cannot silently disagree — the REST twin's ceiling is 100 because
// Store.Search clamps at 100, and copying that number across would over-ask by 2×.
const (
	searchFetchFactor  = 4
	searchMaxFetchRows = 50
	// askContextPages is the grounding window ask_docs builds its prompt from — the "top-3" its
	// own comment names, given a name so the over-fetch and the truncation cannot drift apart.
	askContextPages = 3
)

type pageOut struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	ContentText     string `json:"content_text"`
	SpaceName       string `json:"space_name"`
	URL             string `json:"url"`
	FreshnessStatus string `json:"freshness_status"`

	// A page has TWO independent costs and a derived sum (migration 0018), and this payload used
	// to carry the first one alone — the same half `8b3e1be` removed from the REST search
	// projection. It is the Track half, so a document funded entirely by AI work ON it reported
	// `ai_cost_usd: 0`, byte-identical to a document that genuinely cost nothing, to the agent
	// reading it. And unlike search there is no omitempty here, so it was a concrete numeral zero
	// rather than a gap: a positive assertion, not silence.
	//
	// ⚠ BARE FLOATS, DELIBERATELY, AND NOT BY COPYING THE SEARCH ROW — which uses *float64 because
	// a semantic-only hit has no `pages` row and 0.0 there would be a fabricated zero. pageOut is
	// built at exactly one site from a nil-checked page read through page.scan, which fills all
	// three. There is no "not reported" state here to express.
	//
	// AICostUSD is the cost of the Track ISSUES linked to this page; OwnAICostUSD the cost of AI
	// operations performed ON the document (a LOWER BOUND — see model.Page); TotalAICostUSD their
	// sum, derived once in page.withDerivedTotal so no caller adds them itself.
	AICostUSD      float64 `json:"ai_cost_usd"`
	OwnAICostUSD   float64 `json:"own_ai_cost_usd"`
	TotalAICostUSD float64 `json:"total_ai_cost_usd"`

	LastUpdated string `json:"last_updated"`
	VerifiedBy  string `json:"verified_by,omitempty"`
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
	// SEC-4 VIEW tier, on the page the id or the SLUG actually resolved to. Gated HERE and not in
	// the dispatch switch because the object does not exist until this lookup has run — the slug
	// arm carries only (space_id, slug), so a gate at dispatch would have to read the same row a
	// second time, on the other side of the decision it is making. This is the largest payload of
	// any read tool: content_text is the whole document.
	if !s.canViewPage(ctx, p.ID) {
		return nil, &rpcError{Code: errUnauthorized, Message: "not authorized: this action requires view access"}
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
		OwnAICostUSD:    p.OwnAICostUSD,
		TotalAICostUSD:  p.TotalAICostUSD,
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
	// SEC: the author is the VERIFIED caller, never a client-supplied arg. Tool args are
	// entirely client-controlled, so `created_by` was a self-asserted identity — and
	// permission's resolveAccess treats a creator as an admin. The chokepoint in callTool
	// already resolved and stashed the real actor (authz.WithAuthorized); read it back.
	actor, ok := authz.AuthorizedMember(ctx)
	if !ok {
		return nil, &rpcError{Code: errUnauthorized, Message: "cannot resolve the acting member"}
	}
	// SEC + correctness: the new page's workspace is the VERIFIED workspace the chokepoint resolved from
	// the space and authorized (authz.AuthorizedWorkspace) — never a client-supplied workspace_id, which
	// would let a caller plant a page in a workspace they merely named. page.Store.Create REQUIRES it, so
	// omitting it also made create_page error against a real store (only unit fakes hid it).
	ws, ok := authz.AuthorizedWorkspace(ctx)
	if !ok {
		return nil, &rpcError{Code: errUnauthorized, Message: "cannot resolve the verified workspace"}
	}
	created, err := s.deps.pages.Create(ctx, model.Page{
		SpaceID:     stringArg(args, "space_id", ""),
		WorkspaceID: ws,
		Title:       stringArg(args, "title", ""),
		Content:     pm,
		CreatedBy:   actor,
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
	// SEC: the editor identity is the VERIFIED caller, never the `updated_by` arg. This is
	// not cosmetic — page.Store.Update hands updated_by to the lock guard, and CanEdit
	// allows the write when it equals locked_by, so naming the lock holder edited THROUGH
	// their lock. The chokepoint already stashed the real actor.
	actor, ok := authz.AuthorizedMember(ctx)
	if !ok {
		return nil, &rpcError{Code: errUnauthorized, Message: "cannot resolve the acting member"}
	}
	updates["updated_by"] = actor
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
	// Gather top-3 context pages via the full-text rank query — the same approach the REST
	// /ask endpoint uses, INCLUDING what it does when the query fails (handler.go:223-227
	// returns 500 "search failed"). This used to be `hits, _ :=`, and the sentence above
	// claimed parity it did not have in the only respect that matters when something breaks.
	//
	// A DROPPED ERROR HERE IS NOT A MISSING NUMBER, IT IS AN UNGROUNDED ANSWER. askSystem
	// tells the model to use ONLY the provided documentation; with the search dead, `pages`
	// is empty and the model answers from nothing, and the payload is byte-identical to the
	// one a genuinely empty corpus produces. The caller is an AI agent: it cannot tell "your
	// docs do not cover this" from "the search index is down", and neither can the human
	// reading its answer. Measured both ways in ask_errors_realpg_test.go.
	//
	// The message is fixed and the cause is logged, not returned: the store's error carries
	// SQLSTATE text and relation names, and the caller here is an MCP client.
	// ⚠⚠ THE GROUNDING CORPUS IS GATED HERE AND #79 DID NOT REACH IT. #79 closed the same leak on
	// internal/ai/handler.go's REST /ask — and THIS TOOL NEVER CALLS THAT HANDLER: it runs its own
	// SearchWithRank and hands the rows to the model itself. A sweep by ROUTE finds the REST half
	// and misses this one. Measured: a member with no grant on a private space received its body
	// inside the prompt, under a system message telling the model to use ONLY what it was given,
	// and the page cited back in `sources` with a working deep link.
	//
	// OVER-FETCHED FOR THE SAME REASON search_docs IS, and here it matters more: the window is
	// THREE, so a single hidden row costs a third of the caller's grounding and three hidden rows
	// leave them grounded in NOTHING — which is exactly the ungrounded-answer state the rest of
	// this function exists to refuse.
	askFetch := askContextPages
	if s.access != nil {
		askFetch = askContextPages * searchFetchFactor
	}
	found, err := s.deps.pages.SearchWithRank(ctx, wsID, question, nil, askFetch, 0)
	if err != nil {
		slog.Error("mcp: ask_docs search failed — reporting the failure rather than answering ungrounded",
			slog.String("workspace_id", wsID), slog.Any("err", err))
		return nil, &rpcError{Code: errInternal, Message: "search failed"}
	}
	hits := make([]page.SearchResult, 0, len(found))
	for _, h := range found {
		if !s.canViewPage(ctx, h.Page.ID) {
			continue
		}
		if len(hits) >= askContextPages {
			break
		}
		hits = append(hits, h)
	}
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
	// A FAILED AI CALL REPORTED `"answer": ""` WITH THE SOURCES STILL ATTACHED — which reads
	// as "I read these documents and they say nothing", a claim about the documents, rather
	// than "I could not ask". The REST sibling returns 503 AI_UNAVAILABLE / 502 AI_FAILED
	// (writeAIErr, ai/handler.go:78-93); this is the JSON-RPC shape of the same two.
	//
	// ⚠ THE nil CHECK BELOW IS KEPT BUT IT IS NOT THE GUARD IT LOOKS LIKE, AND MEASURING THAT
	// IS WHY THIS COMMENT EXISTS. deps.ai is an INTERFACE and New takes a *ai.Engine, so a nil
	// engine arrives here as a NON-NIL interface holding a nil pointer — the footgun
	// ai.Engine.IsAvailable documents. `s.deps.ai != nil` is therefore TRUE for a server built
	// by New with no engine; the call proceeds on the nil receiver, IsAvailable returns false,
	// and run() returns ai.ErrUnavailable. Before this change that error was swallowed too, so
	// "Lens is down", "Lens is unconfigured" and "no engine wired at all" were one empty
	// string. All three are errors now.
	//
	// ⚠ AND THE FOURTH CASE — NO ENGINE AT ALL — IS AN ERROR TOO, WHICH IT WAS NOT. New now
	// converts a nil *ai.Engine into a genuinely nil interface, so this branch is REACHABLE
	// rather than the dead check described above. Falling through it left `answer: ""` beside
	// the sources the search half really did find, which is the very shape this comment says was
	// fixed: a claim that the documents say nothing, standing in for "I could not ask". Absent
	// and unavailable are the same answer to the caller, so they share one message.
	if s.deps.ai == nil {
		return nil, &rpcError{Code: errInternal, Message: "the AI service is not available"}
	}
	answer, err := s.deps.ai.AskDocs(ctx, wsID, question, pages)
	if err != nil {
		slog.Error("mcp: ask_docs AI call failed — reporting the failure rather than an empty answer",
			slog.String("workspace_id", wsID), slog.Any("err", err))
		if errors.Is(err, ai.ErrUnavailable) {
			return nil, &rpcError{Code: errInternal, Message: "the AI service is not available"}
		}
		return nil, &rpcError{Code: errInternal, Message: "the AI service failed to answer"}
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
	// AN ABSENT ENGINE IS AN ERROR, NOT AN EMPTY STALE LIST. This guard used to be unreachable
	// (New put a typed nil in the interface), and the `[]` it returned was never the neutral
	// answer it looks like: an empty stale report is the positive claim "nothing in this
	// workspace needs attention", which the SPA paints as a zero on the sidebar.
	// internal/freshness states the rule for its own path in those words and returns errors
	// rather than empty lists; now that New makes this guard live, the tool has to obey it too.
	if s.deps.freshness == nil {
		return nil, &rpcError{Code: errInternal, Message: "the freshness engine is not wired; the stale report is unavailable"}
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
	// SEC: a freshness attestation says "checked by X" — X is the VERIFIED caller, never
	// the `verified_by` arg, which let any caller attest as anyone.
	actor, ok := authz.AuthorizedMember(ctx)
	if !ok {
		return nil, &rpcError{Code: errUnauthorized, Message: "cannot resolve the acting member"}
	}
	if err := s.deps.pages.Verify(ctx, stringArg(args, "page_id", ""), actor); err != nil {
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
	// AN ABSENT ANALYTICS STORE IS AN ERROR, NOT A READERSHIP OF ZERO. This is the MISSING guard
	// the New comment above named as measured-and-not-fixed; the handover is taken here.
	//
	// ⚠ THE FIX THAT COMMENT RECOMMENDED — "a nil-receiver check on analytics.Store … the one
	// that also protects every other caller" — IS INERT FOR THIS ARM, AND THAT WAS MEASURED, NOT
	// REASONED. Since New normalises a nil *analytics.Store into a genuinely nil INTERFACE, the
	// call below panics at METHOD DISPATCH and never enters GetReadStats, so a receiver check
	// there never runs. Probed by adding `if s == nil` to GetReadStats and re-running
	// TestMCP_NilAnalyticsStore_GetPageAnalyticsDoesNotPanic: STILL PANICKED, same message. The
	// guard has to be on the interface, at the call site, which is where freshness and ai already
	// have theirs (toolGetStalePages, toolAskDocs).
	if s.deps.analytics == nil {
		return nil, &rpcError{Code: errInternal, Message: "the analytics store is not wired; page readership is unavailable"}
	}
	days := intArg(args, "days", 7)
	stats, err := s.deps.analytics.GetReadStats(ctx, stringArg(args, "page_id", ""), days)
	if err != nil {
		return nil, err
	}
	// ⚠ AND THE SECOND ARM, WHICH THE INTERFACE GUARD ABOVE CANNOT SEE: a store built by
	// NewStore(nil) is a NON-nil interface over a POOL-LESS Store, and GetReadStats answers that
	// with (nil, nil) — its ONLY nil-nil return, and it is exactly the `if s.pool == nil` arm.
	// The block that followed then rendered three zeroes: {"total_views":0,"unique_viewers":0,
	// "avg_duration_sec":0}, the positive claim "nobody has read this page", made by a service
	// that could not read. Same shape as get_stale_pages' `[]` and ask_docs' `answer: ""`, and
	// the same answer: absent and unreachable are one thing to the caller.
	//
	// ⚠ SCOPED TO THIS SURFACE ON PURPOSE: analytics.Handler.PageStats takes the same (nil, nil)
	// and normalises it to an empty ReadStats with a comment saying why (withEmptyLists, so the
	// SPA gets `[]` and not `null`). Whether the HTTP surface should error instead is a change to
	// a shipped screen's contract and is NOT made here.
	if stats == nil {
		return nil, &rpcError{Code: errInternal, Message: "the analytics store is not wired; page readership is unavailable"}
	}
	out := pageAnalyticsOut{
		TotalViews:     stats.TotalViews,
		UniqueViewers:  stats.UniqueViewers,
		AvgDurationSec: stats.AvgDurationSec,
	}
	if stats.LastViewedAt != nil {
		out.LastViewedAt = stats.LastViewedAt.UTC().Format(time.RFC3339)
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

	// AN ABSENT STORE IS AN ERROR, NOT AN EMPTY TREE. This tool is workspace-keyed, so it clears
	// the chokepoint above and reaches its own body — a different door into the same nil store,
	// and the third site #141's single guard did not reach. Falling through would have answered
	// with `[]`: the positive claim "this workspace has no spaces", which is the shape
	// get_stale_pages' empty list and ask_docs' empty answer already were.
	//
	// BOTH stores are named because both are dereferenced below (spaces.List here, pages.List in
	// the loop) and either being nil produces the same crash.
	if s.deps.spaces == nil || s.deps.pages == nil {
		return nil, &rpcError{Code: errInternal, Message: "the page or space store is not wired; the space tree is unavailable"}
	}
	spaces, err := s.deps.spaces.List(ctx, wsID)
	if err != nil {
		return nil, err
	}
	out := make([]spaceTreeOut, 0, len(spaces))
	for _, sp := range spaces {
		if scopeSpaceID != "" && sp.ID != scopeSpaceID {
			continue
		}
		// The VIEW tier, per SPACE. This tool is workspace-keyed, so it ENUMERATES — no id has to
		// be guessed — and a private space's name is itself a disclosure.
		//
		// ⚠ FILTERED BY SPACE AND NOT ALSO BY PAGE, AND THAT IS THE RULE ENGINE'S PROPERTY RATHER
		// THAN A SHORTCUT: resolveAccess has no DENY — a grant only ever RAISES the level — and
		// CheckPage builds its resourceContext from the SPACE's privacy and creator. A page inside
		// a space the caller may view is therefore viewable by construction, so a per-page pass
		// here could only ever agree with the answer already given.
		if !s.canViewSpace(ctx, sp.ID) {
			continue
		}
		// A FAILED PAGE READ IS NOT AN EMPTY SPACE, AND THIS USED TO BE `pages, _ :=`. The rule is
		// the one stated at the top of this function for the nil store, and it applies with more
		// force here: falling through rendered `{"space_id":…,"name":…,"pages":null}` — MEASURED
		// BYTE-IDENTICAL to the payload a genuinely empty space produces — so "this space has no
		// pages" and "I could not read this space's pages" were ONE answer to an agent that
		// cannot open the database to check. The three sibling reads in this file already refuse
		// exactly this (ask_docs' `hits, _ :=`, get_stale_pages' empty report, the nil stores
		// above); this was the last one left, inside the function that documents the rule.
		//
		// THE WHOLE CALL FAILS rather than the one space being dropped: `spaces.List` above
		// already fails the call, and dropping the space would trade a false "no pages" for a
		// false "no such space" — the same class of claim, one level up. Fixed message, cause
		// logged, following ask_docs: the store's error carries SQLSTATE text and relation names
		// and the caller here is an MCP client.
		pages, err := s.deps.pages.List(ctx, page.PageFilter{SpaceID: sp.ID, Limit: 200})
		if err != nil {
			slog.Error("mcp: get_space_tree page read failed — reporting the failure rather than an empty space",
				slog.String("workspace_id", wsID), slog.String("space_id", sp.ID), slog.Any("err", err))
			return nil, &rpcError{Code: errInternal, Message: "the page read failed; the space tree is unavailable"}
		}
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
