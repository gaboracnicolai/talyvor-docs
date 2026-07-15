package pagelink

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

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
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/pages/{pageID}/links", h.List)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/pages/{pageID}/links", h.Create)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Delete("/pages/{pageID}/links/{issueID}", h.Delete)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListByPage(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []PageLink{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in PageLink
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	in.PageID = chi.URLParam(r, "pageID")
	if in.LinkType == "" {
		in.LinkType = "mention"
	}
	// SEC: PageLink carries workspace_id (the tenancy key on page_links — the table has no
	// FK, it is an opaque Track id) and created_by, and this decodes the whole struct from
	// the body. Both used to insert verbatim, with no authz in this handler at all. Derive
	// them from the parent page, which pageEnf.Require already authorized.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "cannot resolve the workspace for this page")
		return
	}
	actor, ok := permission.ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "cannot resolve the acting member for this page")
		return
	}
	in.WorkspaceID = ws
	in.CreatedBy = actor
	if err := h.store.Upsert(r.Context(), in); err != nil {
		writeErr(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, in)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	issueID := chi.URLParam(r, "issueID")
	if err := h.store.Delete(r.Context(), pageID, issueID); err != nil {
		writeErr(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
