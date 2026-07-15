package templatelib

import (
	"encoding/json"
	"errors"
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
	// Scope to the caller's VERIFIED workspace set, never the {wsID} path
	// param — the path workspace is spoofable; membership is not.
	wsIDs := authz.WorkspaceIDs(r.Context())
	var cat *TemplateCategory
	if c := strings.TrimSpace(r.URL.Query().Get("category")); c != "" {
		t := TemplateCategory(c)
		cat = &t
	}
	search := r.URL.Query().Get("search")
	out, err := h.store.List(r.Context(), wsIDs, cat, search)
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
	var in fromPageBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	// The SOURCE page read is scoped to the caller's VERIFIED set. The NEW
	// template's owner is the {wsID} path workspace, but ONLY if the caller
	// is a verified member of it — otherwise a raw path param could name a
	// foreign workspace to plant a template into. Reject with 404 (no leak).
	wsIDs := authz.WorkspaceIDs(r.Context())
	target := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized write-target: authz.AuthorizeWorkspace below rejects non-members before this workspace owns the new object
	// AuthorizeWorkspace hands back the caller's Membership — use ITS member id. It is the
	// caller's id in THIS workspace, correct for any membership count, unlike ActorOrEmpty
	// which is "" for anyone with != 1 memberships (leaving the object unattributed).
	m, ok := authz.AuthorizeWorkspace(r.Context(), target)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	creator := m.MemberID
	tmpl, err := h.store.CreateFromPage(r.Context(),
		in.PageID, target, creator,
		in.Name, in.Description, in.Category, wsIDs,
	)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
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
	// TEMPLATE read scoped to the caller's VERIFIED set; the NEW page's owner
	// is the {wsID} path workspace, but only if the caller is a verified
	// member (else 404 — a raw path param can't name a foreign target).
	wsIDs := authz.WorkspaceIDs(r.Context())
	target := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized write-target: authz.AuthorizeWorkspace below rejects non-members before this workspace owns the new object
	// AuthorizeWorkspace hands back the caller's Membership — use ITS member id. It is the
	// caller's id in THIS workspace, correct for any membership count, unlike ActorOrEmpty
	// which is "" for anyone with != 1 memberships (leaving the object unattributed).
	m, ok := authz.AuthorizeWorkspace(r.Context(), target)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	creator := m.MemberID
	if creator == "" {
		creator = "user"
	}
	page, err := h.store.UseTemplate(r.Context(), templateID, in.SpaceID, target, creator, wsIDs)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
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
	templateID := chi.URLParam(r, "templateID")
	// Scope to the caller's VERIFIED workspace set, never the {wsID} path
	// param. A template in a workspace the caller doesn't belong to → 404,
	// never deleted cross-tenant (the SEC-4 L2 "deceptive shape" fix).
	err := h.store.Delete(r.Context(), templateID, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		// Built-in deletion returns a typed error; map to 400 so
		// the UI can disable the trash icon for built-ins instead
		// of treating it as a server fault.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
