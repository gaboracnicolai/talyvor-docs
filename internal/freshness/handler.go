package freshness

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	engine  *FreshnessEngine
	pageEnf *permission.Enforcer // A3: by-page access (view)
}

func NewHandler(engine *FreshnessEngine) *Handler { return &Handler{engine: engine} }

// WithAccess wires the A3 access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	r.Get("/workspaces/{wsID}/freshness", h.Workspace)
	// Per-page freshness read → View.
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/freshness", h.Page)
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
	wsID := chi.URLParam(r, "wsID")
	// A4D: authorize the URL workspace against the caller's verified memberships before reporting.
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	reports, err := h.engine.GetStaleReport(r.Context(), wsID)
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
