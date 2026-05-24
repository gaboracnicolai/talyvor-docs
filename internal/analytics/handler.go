package analytics

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Post("/spaces/{spaceID}/pages/{pageID}/view", h.RecordView)
	r.Get("/spaces/{spaceID}/pages/{pageID}/analytics", h.PageStats)
	r.Get("/workspaces/{wsID}/analytics/pages", h.WorkspaceStats)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// daysParam parses the ?days= query param with a sane default + cap.
// 30 days is the spec default; we cap at 365 to avoid pathological
// aggregate scans.
func daysParam(r *http.Request) int {
	d, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if d <= 0 {
		d = 30
	}
	if d > 365 {
		d = 365
	}
	return d
}

func (h *Handler) RecordView(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ViewerID    string `json:"viewer_id"`
		ViewerName  string `json:"viewer_name"`
		Duration    int    `json:"duration_sec"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if in.WorkspaceID == "" {
		// Fall back to a header-supplied workspace if the body
		// omitted it — keeps the client surface minimal.
		in.WorkspaceID = r.Header.Get("X-Talyvor-Workspace")
	}
	err := h.store.RecordView(r.Context(), PageView{
		PageID:      chi.URLParam(r, "pageID"),
		WorkspaceID: in.WorkspaceID,
		ViewerID:    in.ViewerID,
		ViewerName:  in.ViewerName,
		Duration:    in.Duration,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "record failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) PageStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetReadStats(r.Context(), chi.URLParam(r, "pageID"), daysParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	if stats == nil {
		stats = &ReadStats{PageID: chi.URLParam(r, "pageID")}
	}
	writeJSON(w, http.StatusOK, stats)
}

func (h *Handler) WorkspaceStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetWorkspaceStats(r.Context(), chi.URLParam(r, "wsID"), daysParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	if stats == nil {
		stats = &WorkspaceReadStats{}
	}
	writeJSON(w, http.StatusOK, stats)
}
