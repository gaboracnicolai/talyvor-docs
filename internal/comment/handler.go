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
	// DECISION (comment-but-not-edit): comment PARTICIPATION — create/reply/resolve/unresolve/delete —
	// requires the AccessComment tier, while list/stats stay at AccessView (any reader may read the
	// discussion). This makes the "comment" tier real — previously it was gated at AccessView, so it
	// behaved identically to view — giving three distinct tiers: view (read-only), comment (read +
	// discuss, but cannot edit the page), edit (full). Matches the reviewer use case (a reviewer
	// comments without editing). Consequence: a default-view member can no longer comment; to keep
	// commenting open, grant `everyone: comment` on the space/page.
	commentLevel := permission.AccessComment
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

// actorFor resolves the acting member from the GATEWAY-VERIFIED identity, in the workspace
// that owns the parent page. RequireAccess (which gates every route in this package's
// Mount) resolves it via authz.MemberIDForWorkspace and stashes it.
//
// This replaces memberFromReq(r, in.AuthorID), which fell back to the request BODY when
// authz.ActorOrEmpty was empty — and it was empty for every caller with != 1 memberships.
// So a two-workspace member could post a comment authored as anyone (and comment
// authorship is load-bearing: Store.Delete gates on "only the author can delete"), while
// the same member posting honestly got author_id "" and was rejected outright. Resolving
// per-resource-workspace fixes both: there is no fallback, and nothing to forge.
func actorFor(r *http.Request) string {
	m, _ := permission.ActorFromContext(r.Context())
	return m
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

// createBody carries NO author_id: the author is the gateway-verified caller (actorFor),
// never a client claim. A body that still sends author_id is ignored — the field is gone
// and encoding/json drops unknown keys. author_name is display text only, not identity.
type createBody struct {
	Content    string  `json:"content"`
	BlockID    *string `json:"block_id"`
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
		actorFor(r), in.AuthorName, in.Content)
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
		chi.URLParam(r, "pageID"), // the page the route authorized — ties {id} to it
		actorFor(r), in.AuthorName, in.Content, authz.WorkspaceIDs(r.Context()))
	if scoped404(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// resolveBody is empty: who resolved a thread is the verified caller, not a body claim.
// Retained as a type so the decode site keeps its shape if fields are added later.
type resolveBody struct{}

func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	var in resolveBody
	_ = json.NewDecoder(r.Body).Decode(&in)
	resolver := actorFor(r)
	if resolver == "" {
		writeErr(w, http.StatusBadRequest, "resolver required")
		return
	}
	err := h.store.ResolveInWorkspaces(r.Context(), chi.URLParam(r, "id"),
		chi.URLParam(r, "pageID"), resolver, authz.WorkspaceIDs(r.Context()))
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
	err := h.store.UnresolveInWorkspaces(r.Context(), chi.URLParam(r, "id"),
		chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
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
	requester := actorFor(r)
	if requester == "" {
		writeErr(w, http.StatusBadRequest, "verified identity required")
		return
	}
	err := h.store.DeleteInWorkspaces(r.Context(), chi.URLParam(r, "id"),
		chi.URLParam(r, "pageID"), requester, authz.WorkspaceIDs(r.Context()))
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
