package space

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store    *Store
	spaceEnf *permission.Enforcer // A3 within-workspace access guard (nil = unguarded)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 space access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(spaceEnf *permission.Enforcer) *Handler {
	h.spaceEnf = spaceEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// Create + list are workspace-level (no single resource yet; list is filtered in-query). Get
	// requires View; settings-mutation + delete require Admin.
	r.Post("/spaces", h.Create)
	r.Get("/workspaces/{wsID}/spaces", h.List)
	r.With(h.spaceEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}", h.Get)
	r.With(h.spaceEnf.Require(permission.AccessAdmin)).Patch("/spaces/{spaceID}", h.Update)
	r.With(h.spaceEnf.Require(permission.AccessAdmin)).Delete("/spaces/{spaceID}", h.Delete)
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in model.Space
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	// SEC-4: workspace_id arrives in the BODY here (there is no {wsID} path param on this
	// route), so it is attacker-controlled exactly like a URL param would be. Authorize it
	// against the caller's VERIFIED membership set before the insert, or any authenticated
	// caller — including one with zero memberships — can plant a space in any workspace.
	m, ok := authz.AuthorizeWorkspace(r.Context(), in.WorkspaceID)
	if !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
	// Attribution comes from the gateway-verified identity, NEVER the body: permission's
	// resolveAccess treats a space's creator as its admin, so a forged created_by is a
	// self-granted admin rights. m.MemberID is the caller's member id IN this workspace.
	in.CreatedBy = m.MemberID
	out, err := h.store.Create(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	// SEC-4: {wsID} is attacker-controlled — "looks scoped, isn't". Authorize it against
	// the verified membership set before listing.
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line before any store op
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeErr(w, http.StatusForbidden, "FORBIDDEN", "not a member of this workspace")
		return
	}
	out, err := h.store.List(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Space{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	// SEC-4: scope to the caller's verified workspace membership.
	out, err := h.store.GetByIDInWorkspaces(r.Context(), chi.URLParam(r, "spaceID"), authz.WorkspaceIDs(r.Context()))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	out, err := h.store.UpdateInWorkspaces(r.Context(), chi.URLParam(r, "spaceID"), updates, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	err := h.store.DeleteInWorkspaces(r.Context(), chi.URLParam(r, "spaceID"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
