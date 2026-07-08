package comment

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store   *Store
	pageEnf *permission.Enforcer // A3: comment routes gate on the parent page's access (nil = unguarded)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 page access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// DECISION (view-can-comment): comment participation — list/create/reply/resolve/delete — requires
	// only View on the parent page, so view-tier collaborators can discuss without edit. Flip this
	// single `commentLevel` to AccessEdit to make comments an edit-tier action.
	commentLevel := permission.AccessView
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/comments", h.List)
	r.With(h.pageEnf.Require(commentLevel)).Post("/spaces/{spaceID}/pages/{pageID}/comments", h.Create)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/comments/stats", h.Stats)
	r.With(h.pageEnf.Require(commentLevel)).Post("/spaces/{spaceID}/pages/{pageID}/comments/{id}/reply", h.Reply)
	r.With(h.pageEnf.Require(commentLevel)).Post("/spaces/{spaceID}/pages/{pageID}/comments/{id}/resolve", h.Resolve)
	r.With(h.pageEnf.Require(commentLevel)).Delete("/spaces/{spaceID}/pages/{pageID}/comments/{id}/resolve", h.Unresolve)
	r.With(h.pageEnf.Require(commentLevel)).Delete("/spaces/{spaceID}/pages/{pageID}/comments/{id}", h.Delete)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// memberFromReq resolves the actor from the SEC-4 verified identity (never a spoofable
// header), falling back to the body-supplied value only when no verified actor is present
// (e.g. tests without the middleware chain).
func memberFromReq(r *http.Request, fallback string) string {
	if v := authz.ActorOrEmpty(r.Context()); v != "" {
		return v
	}
	return fallback
}

// scoped404 maps a store scope miss (comment's page not in the caller's workspaces) to 404.
func scoped404(w http.ResponseWriter, err error) bool {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return true
	}
	return false
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
	c, err := h.store.ReplyInWorkspaces(r.Context(),
		chi.URLParam(r, "id"),
		memberFromReq(r, in.AuthorID), in.AuthorName, in.Content, authz.WorkspaceIDs(r.Context()))
	if scoped404(w, err) {
		return
	}
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
	err := h.store.ResolveInWorkspaces(r.Context(), chi.URLParam(r, "id"), resolver, authz.WorkspaceIDs(r.Context()))
	if scoped404(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Unresolve(w http.ResponseWriter, r *http.Request) {
	err := h.store.UnresolveInWorkspaces(r.Context(), chi.URLParam(r, "id"), authz.WorkspaceIDs(r.Context()))
	if scoped404(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	requester := authz.ActorOrEmpty(r.Context())
	if requester == "" {
		writeErr(w, http.StatusBadRequest, "verified identity required")
		return
	}
	err := h.store.DeleteInWorkspaces(r.Context(), chi.URLParam(r, "id"), requester, authz.WorkspaceIDs(r.Context()))
	if scoped404(w, err) {
		return
	}
	if err != nil {
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
