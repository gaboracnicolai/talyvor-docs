package analytics

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store   *Store
	pageEnf *permission.Enforcer // A3: by-page access (view/edit)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	r.With(h.pageEnf.Require(permission.AccessView)).Post("/spaces/{spaceID}/pages/{pageID}/view", h.RecordView)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/analytics", h.PageStats)
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
	// No viewer_id / workspace_id: both are derived from the verified caller and the
	// authorized page. viewer_name is display text only, not identity.
	var in struct {
		ViewerName string `json:"viewer_name"`
		Duration   int    `json:"duration_sec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// SEC: the workspace AND the viewer both come from the resource this route already
	// authorized — never the body.
	//
	// workspace_id was overridden here before, but via authz.WorkspaceOrEmpty, which
	// returns "" for a multi-workspace caller and so silently NO-OP'd for exactly those
	// callers, leaving the client's value. viewer_id was not overridden at all, and it
	// feeds COUNT(DISTINCT viewer_id) / GROUP BY viewer_id — so the body could forge who
	// read a page. WorkspaceFromContext / ActorFromContext are correct for any membership
	// count.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	viewer, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the viewing member for this page")
		return
	}
	// RecordViewInWorkspaces scopes the write to the caller's VERIFIED membership set — so the
	// page bump is gated in-method, not solely by this route's pageEnf wiring. A foreign pageID
	// resolves to 404 (no cross-tenant existence oracle).
	err := h.store.RecordViewInWorkspaces(r.Context(), PageView{
		PageID:      chi.URLParam(r, "pageID"),
		WorkspaceID: ws,
		ViewerID:    viewer,
		ViewerName:  in.ViewerName,
		Duration:    in.Duration,
	}, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
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
	wsID := chi.URLParam(r, "wsID")
	// A4D: authorize the URL workspace against the caller's verified memberships.
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "forbidden")
		return
	}
	stats, err := h.store.GetWorkspaceStats(r.Context(), wsID, daysParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stats failed")
		return
	}
	if stats == nil {
		stats = &WorkspaceReadStats{}
	}
	writeJSON(w, http.StatusOK, stats)
}
