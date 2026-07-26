package trackintegration

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
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

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// GetIssue returns the embed-friendly issue ref. When Track isn't
// wired we return {"configured": false} so the frontend can show
// the right empty state.
func (h *Handler) GetIssue(w http.ResponseWriter, r *http.Request) {
	if !h.client.IsConfigured() {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	// SEC-4 L2: {wsID} is caller-supplied and was previously passed STRAIGHT to the Track
	// client, which builds /v1/workspaces/{wsID}/issues/... and sends it under Docs's OWN
	// service API key. That made Docs a confused deputy: Track sees a trusted service
	// credential plus a workspace the caller merely named, so any authenticated Docs user
	// could read another tenant's issue titles and statuses. Authorize against the verified
	// membership set FIRST — the same guard ai/analytics/freshness already apply — so the
	// credential is never spent on a workspace the caller has not proved.
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before any upstream call
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "not a member of this workspace")
		return
	}
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
	// Same confused-deputy shape as GetIssue: SearchIssues builds
	// /v1/workspaces/{wsID}/issues/search and sends Docs's service key.
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line by AuthorizeWorkspace, before any upstream call
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "not a member of this workspace")
		return
	}
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
