package team

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/workspaces/{wsID}/teams", h.List)
	r.Post("/workspaces/{wsID}/teams", h.Create)
	r.Put("/workspaces/{wsID}/teams/{teamID}/members/{memberID}", h.AddMember)
	r.Delete("/workspaces/{wsID}/teams/{teamID}/members/{memberID}", h.RemoveMember)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// caller authorizes {wsID} against the verified memberships and returns it with the caller's
// member id there — nothing is read from the request body.
func caller(w http.ResponseWriter, r *http.Request) (wsID, memberID string, ok bool) {
	wsID = chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line before any store op
	m, ok := authz.AuthorizeWorkspace(r.Context(), wsID)
	if !ok || m.MemberID == "" {
		writeErr(w, http.StatusForbidden, "not a member of this workspace")
		return "", "", false
	}
	return wsID, m.MemberID, true
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	ws, me, ok := caller(w, r)
	if !ok {
		return
	}
	out, err := h.store.List(r.Context(), ws, me)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list teams")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	ws, me, ok := caller(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	t, err := h.store.Create(r.Context(), ws, in.Name, me)
	switch {
	case errors.Is(err, ErrInvalidName):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrDuplicate):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "could not create the team")
	default:
		writeJSON(w, http.StatusCreated, t)
	}
}

func (h *Handler) AddMember(w http.ResponseWriter, r *http.Request) {
	h.roster(w, r, h.store.AddMember, true)
}

func (h *Handler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	h.roster(w, r, h.store.RemoveMember, false)
}

// roster runs one membership change as the verified caller, who must have created the team.
func (h *Handler) roster(w http.ResponseWriter, r *http.Request,
	op func(ctx context.Context, wsID, teamID, memberID, actor string) error, member bool) {
	ws, me, ok := caller(w, r)
	if !ok {
		return
	}
	err := op(r.Context(), ws, chi.URLParam(r, "teamID"), chi.URLParam(r, "memberID"), me)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, ErrUnknownMember):
		writeErr(w, http.StatusBadRequest, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "could not change the team")
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"member": member})
	}
}
