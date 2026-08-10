package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/ratelimit"
)

// PageSearcher is the page-store dependency the /ask endpoint needs
// to gather context pages. Narrow on purpose — the handler should
// not be able to mutate pages.
type PageSearcher interface {
	Search(ctx context.Context, workspaceID, query string, limit int) ([]model.Page, error)
}

// pageReader authorizes a READ of ONE page for the verified caller. *spaceauth.Authorizer
// satisfies it — the same shipped primitive internal/search and page.Handler use, so this package
// introduces NO new access model.
type pageReader interface {
	AuthorizePageRead(ctx context.Context, pageID string) (found, canView bool)
}

type Handler struct {
	engine *Engine
	pages  PageSearcher
	// limit throttles every LLM route per VERIFIED workspace. nil = unthrottled, which is
	// the pre-hardening behaviour and is retained ONLY so tests can mount the handler bare;
	// main.go always wires it. See WithRateLimit.
	limit *ratelimit.Limiter
	// access gates the pages /ask is allowed to ground an answer in. See WithPageRead.
	access pageReader
}

func NewHandler(engine *Engine, pages PageSearcher) *Handler {
	return &Handler{engine: engine, pages: pages}
}

// WithRateLimit attaches the per-workspace LLM rate limiter. Every route here proxies to
// Lens on Docs's single service key with no balance or quota check anywhere in the
// codebase, so without this a workspace can drive unbounded spend. The limiter keys on the
// VERIFIED workspace (it authorizes {wsID} itself, before the handler's own check) — never
// the raw path param, or a caller names its own bucket and evades the ceiling.
func (h *Handler) WithRateLimit(l *ratelimit.Limiter) *Handler {
	h.limit = l
	return h
}

// WithPageRead attaches the per-page read gate /ask needs to decide which documents may ground an
// answer for THIS caller. `Store.Search` scopes to the workspace and nothing else, and the route
// names a QUESTION rather than an id, so no chi URL-param resolver (permission.RequireAccess) can
// gate it — the same reason internal/search and page.Handler both needed this primitive.
//
// ⚠ NIL REFUSES THE REQUEST RATHER THAN ANSWERING, AND THAT DELIBERATELY DIFFERS FROM BOTH
// SIBLINGS. internal/search treats nil as UNFILTERED; page.Handler treats it as an EMPTY list.
// Neither is available here, because this route's output is PROSE:
//
//   - unfiltered is the defect (privatespace_realpg_test.go);
//   - an empty context set is not an empty response — Engine.AskDocs still runs, and the model
//     answers from zero grounding under a system prompt telling it to use only the provided
//     documentation. That answer is byte-indistinguishable from "your documentation does not
//     cover this", which is exactly the failure #57 closed at the MCP ask_docs tool.
//
// So an unwired gate must be LOUD. It is a server misconfiguration, reported as one.
func (h *Handler) WithPageRead(a pageReader) *Handler {
	h.access = a
	return h
}

// limited wraps a route in the workspace limiter. A nil limiter yields the bare handler,
// matching Enforcer.Require's nil-receiver convention.
func (h *Handler) limited(next http.HandlerFunc) http.HandlerFunc {
	if h.limit == nil {
		return next
	}
	wrapped := h.limit.WorkspaceLimit("wsID")(next)
	return wrapped.ServeHTTP
}

// attributable authorizes the BODY-NAMED page an operation's cost will be bound to, and reports
// whether the request may proceed. It writes the refusal itself.
//
// ⚠ THE OBJECT COMES FROM THE BODY, WHICH IS WHY NO ROUTE-SHAPED GATE REACHES IT. These five
// routes are `/workspaces/{wsID}/ai/…`: the only id in the path is the workspace, so
// permission.RequireAccess has nothing to resolve and internal/routeguard's class — a gated route
// whose handler must read its enforcer's param — does not describe them at all. The page arrives
// as a JSON field, and `page_id` is not decoration: Engine.run binds it to Lens's request id and
// page.Store.PriceAISpend later does `UPDATE pages SET own_ai_cost_usd = … WHERE p.id =
// priced.page_id` — by bare id, with no workspace predicate on that path. Unchecked, a member of
// one workspace could name a page in ANOTHER and land real money on a document they have never
// been able to open. Measured through these routes on real Postgres in
// bodypage_attribution_realpg_test.go: $49.36 across a tenant boundary, $31.08 onto a private
// space in the caller's own workspace.
//
// This is the same gate, in the same shape and with the same status codes, that
// templatelib.FromPage applies to ITS body-named page_id, and it is the same primitive — the
// handler already holds it for Ask's grounding filter. Unresolvable or foreign → 404 (no
// existence oracle); resolvable but under View → 403.
//
// IT RUNS BEFORE THE COMPLETION. Refusing after the Lens call would turn misattributed spend into
// unattributed spend that the workspace still pays for; refusing before it costs nothing.
//
// AN EMPTY page_id IS ALLOWED AND GATED BY NOTHING, deliberately. Engine.run documents the two
// operations that always pass it empty (ask, search) and SuggestTitle passes it empty before a
// page's first save; there is no object to authorize and nothing will be bound.
//
// A NIL GATE REFUSES, matching Ask. main.go always wires it, so this is a report rather than the
// only thing standing between an unwired deployment and the leak — but binding blind is exactly
// the defect this closes, and omission must not restore it.
func (h *Handler) attributable(w http.ResponseWriter, r *http.Request, pageID string) bool {
	if pageID == "" {
		return true
	}
	if h.access == nil {
		slog.Error("ai: no page-read gate wired — refusing a page_id-carrying request rather " +
			"than binding its cost blind (cmd/docs/main.go must call aiHandler.WithPageRead)")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "attribution unavailable"})
		return false
	}
	found, canView := h.access.AuthorizePageRead(r.Context(), pageID)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return false
	}
	if !canView {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "insufficient access: attributing an AI operation requires view access to page_id",
		})
		return false
	}
	return true
}

func (h *Handler) Mount(r chi.Router) {
	// Every one of these reaches Lens. All five are rate-limited per verified workspace.
	r.Post("/workspaces/{wsID}/ai/write", h.limited(h.Write))
	r.Post("/workspaces/{wsID}/ai/transform", h.limited(h.Transform))
	r.Post("/workspaces/{wsID}/ai/translate", h.limited(h.Translate))
	r.Post("/workspaces/{wsID}/ai/ask", h.limited(h.Ask))
	r.Post("/workspaces/{wsID}/ai/suggest-title", h.limited(h.SuggestTitle))
}

// userMessage is what we return to the client when the engine fails.
// It never leaks raw upstream error strings — those frequently expose
// the Lens URL or API behaviour and aren't actionable for the editor.
const userMessage = "AI unavailable. Check Lens configuration."

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAIErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": userMessage,
			"code":  "AI_UNAVAILABLE",
		})
		return
	}
	// Bucket everything else as a degraded-AI condition. We log the
	// underlying error one level up so operators can diagnose, but
	// don't bubble it to the user.
	writeJSON(w, http.StatusBadGateway, map[string]string{
		"error": userMessage,
		"code":  "AI_FAILED",
	})
}

// ─── /write ────────────────────────────────────────────────

func (h *Handler) Write(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before the value reaches the engine or Lens
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		Prompt  string `json:"prompt"`
		Context string `json:"context"`
		PageID  string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt required"})
		return
	}
	if !h.attributable(w, r, in.PageID) {
		return
	}
	out, err := h.engine.WriteWithAI(r.Context(), wsID, in.Prompt, in.Context, in.PageID)
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

// ─── /transform ────────────────────────────────────────────

func (h *Handler) Transform(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before the value reaches the engine or Lens
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		Action string `json:"action"`
		Text   string `json:"text"`
		PageID string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if !h.attributable(w, r, in.PageID) {
		return
	}
	var (
		out string
		err error
	)
	switch in.Action {
	case "summarize":
		out, err = h.engine.Summarize(r.Context(), wsID, in.Text, in.PageID)
	case "grammar":
		out, err = h.engine.FixGrammar(r.Context(), wsID, in.Text, in.PageID)
	case "shorter":
		out, err = h.engine.MakeShorter(r.Context(), wsID, in.Text, in.PageID)
	case "longer":
		out, err = h.engine.MakeLonger(r.Context(), wsID, in.Text, in.PageID)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown action: %s", in.Action),
		})
		return
	}
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

// ─── /translate ────────────────────────────────────────────

func (h *Handler) Translate(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before the value reaches the engine or Lens
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		PageID   string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if !h.attributable(w, r, in.PageID) {
		return
	}
	out, err := h.engine.Translate(r.Context(), wsID, in.Text, in.Language, in.PageID)
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

// ─── /ask ──────────────────────────────────────────────────

type askSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type askResponse struct {
	Answer  string      `json:"answer"`
	Sources []askSource `json:"sources"`
}

// askContextPages / askFetchFactor / askFetchRows size the grounding window.
//
// askContextPages is the number of pages that reach the prompt — unchanged at 3: "anything past
// that bloats the prompt for little extra recall". askFetchRows is what the store is ASKED for, so
// that rows the caller may not read can be dropped without shrinking the window below the context
// size. 12 is inside page.Store.Search's own clamp of 100, so the number the store is given is the
// number it uses.
const (
	askContextPages = 3
	askFetchFactor  = 4
	askFetchRows    = askContextPages * askFetchFactor
)

// visibleTo drops every page the caller may not VIEW, asking the same engine the by-id page routes
// ask.
//
// A nil gate does not reach here in production — Ask refuses before the store op — but this fails
// closed anyway, matching page.Handler.visibleTo. The two together are what make the refusal a
// REPORT rather than the only thing standing between an unwired deployment and the leak: delete
// the refusal and /ask answers from nothing, which is loudly wrong; delete this and it answers
// from everything, which is not.
func (h *Handler) visibleTo(ctx context.Context, pages []model.Page) []model.Page {
	if h.access == nil {
		return nil
	}
	out := make([]model.Page, 0, len(pages))
	for _, p := range pages {
		found, canView := h.access.AuthorizePageRead(ctx, p.ID)
		if !found || !canView {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before the value reaches the engine or Lens
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		Question string `json:"question"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if strings.TrimSpace(in.Question) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "question required"})
		return
	}
	// THE GATE IS REQUIRED, AND ITS ABSENCE IS REPORTED RATHER THAN ANSWERED AROUND. See
	// WithPageRead: with no gate the only two alternatives are grounding the answer in every
	// page in the workspace (the defect) or grounding it in none, and the second is a confident
	// answer from an empty corpus that reads exactly like "the documentation does not cover
	// this".
	if h.access == nil {
		slog.Error("ai: /ask has no page-read gate wired — refusing rather than answering " +
			"from an unfiltered or empty corpus (cmd/docs/main.go must call aiHandler.WithPageRead)")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ask unavailable"})
		return
	}
	// OVER-FETCH, BECAUSE THE PAGES THE CALLER MAY NOT READ ARE DROPPED AFTER THE SQL LIMIT.
	// Measured: three pages in a private space sorting above one public page leave a denied
	// caller with ZERO grounding if the window is the context size — and the model answers
	// anyway. It is a mitigation, not a guarantee: a run of more than
	// (askFetchFactor-1)×askContextPages consecutive unreadable rows still under-fills, and the
	// response has never carried a count, so nothing here newly misreports one.
	pages, err := h.pages.Search(r.Context(), wsID, in.Question, askFetchRows)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}
	// Drop every page the caller may not VIEW *before* truncating to the context size — a
	// document they cannot open must not consume one of their grounding slots, and must not be
	// quoted to the model or cited back to them either way.
	//
	// A page that is filtered out is indistinguishable from one that did not match: both simply
	// are not there. The empty-corpus answer a denied caller may get is the same answer the
	// corpus genuinely having nothing produces, which is the correct amount of information.
	pages = h.visibleTo(r.Context(), pages)
	if len(pages) > askContextPages {
		pages = pages[:askContextPages]
	}
	ctxPages := make([]PageContext, 0, len(pages))
	sources := make([]askSource, 0, len(pages))
	for _, p := range pages {
		url := pageURL(p)
		ctxPages = append(ctxPages, PageContext{
			Title:   p.Title,
			Content: p.ContentText,
			URL:     url,
		})
		sources = append(sources, askSource{Title: p.Title, URL: url})
	}
	answer, err := h.engine.AskDocs(r.Context(), wsID, in.Question, ctxPages)
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, askResponse{Answer: answer, Sources: sources})
}

// pageURL builds a relative URL to the page within the Docs frontend.
// The host is unknown to the server, so we return a path the SPA can
// resolve against its own origin.
func pageURL(p model.Page) string {
	if p.Slug != "" && p.SpaceID != "" {
		return fmt.Sprintf("/spaces/%s/pages/%s", p.SpaceID, p.ID)
	}
	return ""
}

// ─── /suggest-title ────────────────────────────────────────

func (h *Handler) SuggestTitle(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before the value reaches the engine or Lens
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	var in struct {
		Content string `json:"content"`
		PageID  string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if !h.attributable(w, r, in.PageID) {
		return
	}
	title, err := h.engine.SuggestTitle(r.Context(), wsID, in.Content, in.PageID)
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"title": title})
}
