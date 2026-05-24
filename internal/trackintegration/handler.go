package trackintegration

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ client *Client }

func NewHandler(c *Client) *Handler { return &Handler{client: c} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/workspaces/{wsID}/track/issues/{issueID}", h.GetIssue)
	r.Get("/workspaces/{wsID}/track/search", h.Search)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// GetIssue returns the embed-friendly issue ref. When Track isn't
// wired we return {"configured": false} so the frontend can show
// the right empty state.
func (h *Handler) GetIssue(w http.ResponseWriter, r *http.Request) {
	if !h.client.IsConfigured() {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	wsID := chi.URLParam(r, "wsID")
	issueID := chi.URLParam(r, "issueID")
	ref, err := h.client.GetIssue(r.Context(), wsID, issueID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"available":  false,
			"error":      err.Error(),
		})
		return
	}
	if ref == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"available":  false,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"available":  true,
		"issue":      ref,
	})
}

func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	if !h.client.IsConfigured() {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"issues":     []IssueRef{},
		})
		return
	}
	wsID := chi.URLParam(r, "wsID")
	q := r.URL.Query().Get("q")
	out, _ := h.client.SearchIssues(r.Context(), wsID, q)
	if out == nil {
		out = []IssueRef{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"issues":     out,
	})
}
