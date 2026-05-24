package pagelink

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/pages/{pageID}/links", h.List)
	r.Post("/pages/{pageID}/links", h.Create)
	r.Delete("/pages/{pageID}/links/{issueID}", h.Delete)
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
