package freshness

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ engine *FreshnessEngine }

func NewHandler(engine *FreshnessEngine) *Handler { return &Handler{engine: engine} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/workspaces/{wsID}/freshness", h.Workspace)
	r.Get("/spaces/{spaceID}/pages/{pageID}/freshness", h.Page)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) Workspace(w http.ResponseWriter, r *http.Request) {
	reports, err := h.engine.GetStaleReport(r.Context(), chi.URLParam(r, "wsID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "freshness report failed")
		return
	}
	if reports == nil {
		reports = []FreshnessReport{}
	}
	writeJSON(w, http.StatusOK, reports)
}

func (h *Handler) Page(w http.ResponseWriter, r *http.Request) {
	rep, err := h.engine.GetStatus(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "freshness check failed")
		return
	}
	if rep == nil {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
