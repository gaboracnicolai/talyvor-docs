package permission

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/spaces/{spaceID}/permissions", h.listSpace)
	r.Post("/spaces/{spaceID}/permissions", h.grantSpace)
	r.Delete("/spaces/{spaceID}/permissions/{permID}", h.delete)

	r.Get("/spaces/{spaceID}/pages/{pageID}/permissions", h.listPage)
	r.Post("/spaces/{spaceID}/pages/{pageID}/permissions", h.grantPage)
	r.Delete("/spaces/{spaceID}/pages/{pageID}/permissions/{permID}", h.delete)
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
	out, err := h.store.ListForResource(r.Context(), ResourceSpace, chi.URLParam(r, "spaceID"))
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
	out, err := h.store.ListForResource(r.Context(), ResourcePage, chi.URLParam(r, "pageID"))
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
	if in.WorkspaceID == "" {
		in.WorkspaceID = r.Header.Get("X-Talyvor-Workspace")
	}
	p := Permission{
		ResourceType: resType,
		ResourceID:   resID,
		SubjectType:  in.SubjectType,
		SubjectID:    in.SubjectID,
		Access:       in.Access,
		WorkspaceID:  in.WorkspaceID,
		GrantedBy:    r.Header.Get("X-Member-Id"),
	}
	if err := h.store.Grant(r.Context(), p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.store.RevokeByID(r.Context(), chi.URLParam(r, "permID")); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
