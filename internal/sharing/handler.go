package sharing

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

// PageContentLoader gives the public-share handler a way to load a
// page's renderable content without dragging the page package into
// sharing. The host wires its existing page.Store into this
// signature.
type PageContentLoader func(ctx context.Context, pageID string) (*PublicPage, error)

// PublicPage is the lean shape returned by the public /s/:token
// endpoint. We deliberately omit edit metadata (created_by,
// view_count, AI cost, etc) — public readers don't need it.
type PublicPage struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Icon        string `json:"icon"`
	Content     string `json:"content"`
	ContentText string `json:"content_text"`
	UpdatedAt   string `json:"updated_at"`
}

type Handler struct {
	store   *Store
	page    PageContentLoader
	pageEnf *permission.Enforcer // A3: sharing admin gates on the parent page (nil = unguarded)
}

func NewHandler(store *Store, page PageContentLoader) *Handler {
	return &Handler{store: store, page: page}
}

// WithAccess wires the A3 page access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// Creating/listing/revoking share links is an admin action on the page — share links are public
	// access-granting tokens, so only Admins may mint, enumerate, or revoke them.
	r.With(h.pageEnf.Require(permission.AccessAdmin)).Post("/spaces/{spaceID}/pages/{pageID}/share", h.Create)
	r.With(h.pageEnf.Require(permission.AccessAdmin)).Get("/spaces/{spaceID}/pages/{pageID}/share", h.List)
	r.With(h.pageEnf.Require(permission.AccessAdmin)).Delete("/spaces/{spaceID}/pages/{pageID}/share/{id}", h.Revoke)
}

// MountPublic mounts the no-auth public viewer at /v1/public/s/:token — authenticated by the share
// token itself (NOT membership), so it is NOT behind RequireAccess. Public-by-design.
func (h *Handler) MountPublic(r chi.Router) {
	r.Get("/public/s/{token}", h.Public)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type createReq struct {
	Access        permission.AccessLevel `json:"access"`
	ExpiresInDays int                    `json:"expires_in_days"`
	Password      string                 `json:"password"`
	WorkspaceID   string                 `json:"workspace_id"`
}

type createResp struct {
	Link     *ShareLink `json:"link"`
	ShareURL string     `json:"share_url"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in createReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if ws := authz.WorkspaceOrEmpty(r.Context()); ws != "" {
		in.WorkspaceID = ws
	}
	if in.Access == "" {
		in.Access = permission.AccessView
	}
	var expiresAt *time.Time
	if in.ExpiresInDays > 0 {
		t := time.Now().UTC().Add(time.Duration(in.ExpiresInDays) * 24 * time.Hour)
		expiresAt = &t
	}
	link, err := h.store.Create(r.Context(),
		chi.URLParam(r, "pageID"),
		in.WorkspaceID,
		authz.ActorOrEmpty(r.Context()),
		in.Access,
		expiresAt,
		in.Password,
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The share URL is built host-relative; the client / SPA
	// resolves it against its own origin. We don't know the Docs
	// public hostname server-side.
	writeJSON(w, http.StatusCreated, createResp{
		Link:     link,
		ShareURL: "/s/" + link.Token,
	})
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListByPage(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	if out == nil {
		out = []ShareLink{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Revoke(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Revoke(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// Public is the no-auth viewer. The X-Powered-By header surfaces
// the Phase 8 contract ("Powered by Talyvor Docs"); the response
// body returns the lean PublicPage projection.
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Powered-By", "Talyvor Docs")
	password := r.URL.Query().Get("password")
	link, err := h.store.Validate(r.Context(), chi.URLParam(r, "token"), password)
	if err != nil {
		// Map specific error strings to user-meaningful HTTP codes.
		switch err.Error() {
		case "sharing: password required":
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error":         "password required",
				"requires_pass": "true",
			})
		case "sharing: password mismatch":
			writeErr(w, http.StatusUnauthorized, "incorrect password")
		case "sharing: link expired":
			writeErr(w, http.StatusGone, "this link has expired")
		default:
			writeErr(w, http.StatusNotFound, "link not found")
		}
		return
	}
	if h.page == nil {
		writeErr(w, http.StatusInternalServerError, "page loader missing")
		return
	}
	page, err := h.page(r.Context(), link.PageID)
	if err != nil || page == nil {
		writeErr(w, http.StatusNotFound, "page not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"page":         page,
		"access":       link.Access,
		"has_password": link.HasPassword,
		"expires_at":   link.ExpiresAt,
		"powered_by":   "Talyvor Docs",
	})
}
