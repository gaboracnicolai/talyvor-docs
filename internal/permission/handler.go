package permission

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
)

type Handler struct {
	store    *Store
	spaceEnf *Enforcer // A3 within-workspace access guard for space-scoped perm routes (nil = unguarded)
	pageEnf  *Enforcer // A3 within-workspace access guard for page-scoped perm routes (nil = unguarded)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcers. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(spaceEnf, pageEnf *Enforcer) *Handler {
	h.spaceEnf = spaceEnf
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// A3: viewing, granting, and revoking access all reveal or change who can reach a resource, so
	// every permission-management route is gated at Admin on the relevant resource's enforcer.
	r.With(h.spaceEnf.Require(AccessAdmin)).Method(http.MethodGet, "/spaces/{spaceID}/permissions", http.HandlerFunc(h.listSpace))
	r.With(h.spaceEnf.Require(AccessAdmin)).Method(http.MethodPost, "/spaces/{spaceID}/permissions", http.HandlerFunc(h.grantSpace))
	r.With(h.spaceEnf.Require(AccessAdmin)).Method(http.MethodDelete, "/spaces/{spaceID}/permissions/{permID}", http.HandlerFunc(h.delete))

	r.With(h.pageEnf.Require(AccessAdmin)).Method(http.MethodGet, "/spaces/{spaceID}/pages/{pageID}/permissions", http.HandlerFunc(h.listPage))
	r.With(h.pageEnf.Require(AccessAdmin)).Method(http.MethodPost, "/spaces/{spaceID}/pages/{pageID}/permissions", http.HandlerFunc(h.grantPage))
	r.With(h.pageEnf.Require(AccessAdmin)).Method(http.MethodDelete, "/spaces/{spaceID}/pages/{pageID}/permissions/{permID}", http.HandlerFunc(h.delete))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) listSpace(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListForResource(r.Context(), ResourceSpace, chi.URLParam(r, "spaceID"), authz.WorkspaceIDs(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	if out == nil {
		out = []Permission{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listPage(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListForResource(r.Context(), ResourcePage, chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	if out == nil {
		out = []Permission{}
	}
	writeJSON(w, http.StatusOK, out)
}

type grantBody struct {
	SubjectType string      `json:"subject_type"`
	SubjectID   string      `json:"subject_id"`
	Access      AccessLevel `json:"access"`
	WorkspaceID string      `json:"workspace_id"`
}

func (h *Handler) grantSpace(w http.ResponseWriter, r *http.Request) {
	h.grant(w, r, ResourceSpace, chi.URLParam(r, "spaceID"))
}
func (h *Handler) grantPage(w http.ResponseWriter, r *http.Request) {
	h.grant(w, r, ResourcePage, chi.URLParam(r, "pageID"))
}

func (h *Handler) grant(w http.ResponseWriter, r *http.Request, resType ResourceType, resID string) {
	var in grantBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// SEC-4: stamp the grant's workspace from the caller's VERIFIED membership, overriding any
	// client-supplied in.WorkspaceID — a grant can never be written into a foreign workspace.
	// (Verifying the target resource_id itself belongs to that workspace is a separate fix, out
	// of scope here: this closes the workspace_id-spoof vector on the write path.)
	// WorkspaceOrEmpty no-ops for a multi-workspace caller, which left the BODY's
	// workspace_id on the grant. Derive it from the resource the route authorized.
	ws, ok := WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this resource")
		return
	}
	in.WorkspaceID = ws
	grantedBy, ok := ActorFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this resource")
		return
	}
	p := Permission{
		ResourceType: resType,
		ResourceID:   resID,
		SubjectType:  in.SubjectType,
		SubjectID:    in.SubjectID,
		Access:       in.Access,
		WorkspaceID:  in.WorkspaceID,
		GrantedBy:    grantedBy,
	}
	if err := h.store.Grant(r.Context(), p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	err := h.store.RevokeByID(r.Context(), chi.URLParam(r, "permID"), wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
