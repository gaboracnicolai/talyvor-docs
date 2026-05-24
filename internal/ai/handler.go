package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/model"
)

// PageSearcher is the page-store dependency the /ask endpoint needs
// to gather context pages. Narrow on purpose — the handler should
// not be able to mutate pages.
type PageSearcher interface {
	Search(ctx context.Context, workspaceID, query string, limit int) ([]model.Page, error)
}

type Handler struct {
	engine *Engine
	pages  PageSearcher
}

func NewHandler(engine *Engine, pages PageSearcher) *Handler {
	return &Handler{engine: engine, pages: pages}
}

func (h *Handler) Mount(r chi.Router) {
	r.Post("/workspaces/{wsID}/ai/write", h.Write)
	r.Post("/workspaces/{wsID}/ai/transform", h.Transform)
	r.Post("/workspaces/{wsID}/ai/translate", h.Translate)
	r.Post("/workspaces/{wsID}/ai/ask", h.Ask)
	r.Post("/workspaces/{wsID}/ai/suggest-title", h.SuggestTitle)
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
	wsID := chi.URLParam(r, "wsID")
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
	out, err := h.engine.WriteWithAI(r.Context(), wsID, in.Prompt, in.Context)
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

// ─── /transform ────────────────────────────────────────────

func (h *Handler) Transform(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	var in struct {
		Action string `json:"action"`
		Text   string `json:"text"`
		PageID string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	var (
		out string
		err error
	)
	switch in.Action {
	case "summarize":
		out, err = h.engine.Summarize(r.Context(), wsID, in.Text)
	case "grammar":
		out, err = h.engine.FixGrammar(r.Context(), wsID, in.Text)
	case "shorter":
		out, err = h.engine.MakeShorter(r.Context(), wsID, in.Text)
	case "longer":
		out, err = h.engine.MakeLonger(r.Context(), wsID, in.Text)
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
	wsID := chi.URLParam(r, "wsID")
	var in struct {
		Text     string `json:"text"`
		Language string `json:"language"`
		PageID   string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	out, err := h.engine.Translate(r.Context(), wsID, in.Text, in.Language)
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

func (h *Handler) Ask(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
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
	// Top-3 full-text matches form the model's grounding context.
	// Anything past that bloats the prompt for little extra recall.
	pages, err := h.pages.Search(r.Context(), wsID, in.Question, 3)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
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
	wsID := chi.URLParam(r, "wsID")
	var in struct {
		Content string `json:"content"`
		PageID  string `json:"page_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	title, err := h.engine.SuggestTitle(r.Context(), wsID, in.Content)
	if err != nil {
		writeAIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"title": title})
}
