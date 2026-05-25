package comment

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/spaces/{spaceID}/pages/{pageID}/comments", h.List)
	r.Post("/spaces/{spaceID}/pages/{pageID}/comments", h.Create)
	r.Get("/spaces/{spaceID}/pages/{pageID}/comments/stats", h.Stats)
	r.Post("/spaces/{spaceID}/pages/{pageID}/comments/{id}/reply", h.Reply)
	r.Post("/spaces/{spaceID}/pages/{pageID}/comments/{id}/resolve", h.Resolve)
	r.Delete("/spaces/{spaceID}/pages/{pageID}/comments/{id}/resolve", h.Unresolve)
	r.Delete("/spaces/{spaceID}/pages/{pageID}/comments/{id}", h.Delete)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// memberFromReq prefers the X-Member-Id header, falls back to the
// value the body supplied. Header is preferred because it can't be
// spoofed by a malicious client editor.
func memberFromReq(r *http.Request, fallback string) string {
	if v := r.Header.Get("X-Member-Id"); v != "" {
		return v
	}
	return fallback
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	includeResolved := r.URL.Query().Get("include_resolved") == "true"
	out, err := h.store.ListByPage(r.Context(), chi.URLParam(r, "pageID"), includeResolved)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []Comment{}
	}
	writeJSON(w, http.StatusOK, out)
}

type createBody struct {
	Content    string  `json:"content"`
	BlockID    *string `json:"block_id"`
	AuthorID   string  `json:"author_id"`
	AuthorName string  `json:"author_name"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	var in createBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	c, err := h.store.Create(r.Context(),
		pageID, in.BlockID,
		memberFromReq(r, in.AuthorID), in.AuthorName, in.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

type replyBody struct {
	Content    string `json:"content"`
	AuthorID   string `json:"author_id"`
	AuthorName string `json:"author_name"`
}

func (h *Handler) Reply(w http.ResponseWriter, r *http.Request) {
	var in replyBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	c, err := h.store.Reply(r.Context(),
		chi.URLParam(r, "id"),
		memberFromReq(r, in.AuthorID), in.AuthorName, in.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

type resolveBody struct {
	ResolvedBy string `json:"resolved_by"`
}

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	var in resolveBody
	_ = json.NewDecoder(r.Body).Decode(&in)
	resolver := memberFromReq(r, in.ResolvedBy)
	if resolver == "" {
		writeErr(w, http.StatusBadRequest, "resolver required")
		return
	}
	if err := h.store.Resolve(r.Context(), chi.URLParam(r, "id"), resolver); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Unresolve(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Unresolve(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	requester := r.Header.Get("X-Member-Id")
	if requester == "" {
		writeErr(w, http.StatusBadRequest, "X-Member-Id required")
		return
	}
	if err := h.store.Delete(r.Context(), chi.URLParam(r, "id"), requester); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.store.GetStats(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}
