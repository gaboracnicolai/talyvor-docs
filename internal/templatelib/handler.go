package templatelib

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Get("/workspaces/{wsID}/template-library", h.List)
	r.Post("/workspaces/{wsID}/template-library/from-page", h.FromPage)
	r.Post("/workspaces/{wsID}/template-library/{templateID}/use", h.Use)
	r.Delete("/workspaces/{wsID}/template-library/{templateID}", h.Delete)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	var cat *TemplateCategory
	if c := strings.TrimSpace(r.URL.Query().Get("category")); c != "" {
		t := TemplateCategory(c)
		cat = &t
	}
	search := r.URL.Query().Get("search")
	out, err := h.store.List(r.Context(), wsID, cat, search)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []LibraryTemplate{}
	}
	writeJSON(w, http.StatusOK, out)
}

type fromPageBody struct {
	PageID      string           `json:"page_id"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Category    TemplateCategory `json:"category"`
}

func (h *Handler) FromPage(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	var in fromPageBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	creator := authz.ActorOrEmpty(r.Context())
	tmpl, err := h.store.CreateFromPage(r.Context(),
		in.PageID, wsID, creator,
		in.Name, in.Description, in.Category,
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}

type useBody struct {
	SpaceID string `json:"space_id"`
}

func (h *Handler) Use(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	templateID := chi.URLParam(r, "templateID")
	var in useBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if in.SpaceID == "" {
		writeErr(w, http.StatusBadRequest, "space_id is required")
		return
	}
	creator := authz.ActorOrEmpty(r.Context())
	if creator == "" {
		creator = "user"
	}
	page, err := h.store.UseTemplate(r.Context(), templateID, in.SpaceID, wsID, creator)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"page_id":  page.ID,
		"page_url": "/spaces/" + page.SpaceID + "/pages/" + page.ID,
	})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	templateID := chi.URLParam(r, "templateID")
	if err := h.store.Delete(r.Context(), templateID, wsID); err != nil {
		// Built-in deletion returns a typed error; map to 400 so
		// the UI can disable the trash icon for built-ins instead
		// of treating it as a server fault.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
