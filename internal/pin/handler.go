package pin

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

// recentCount is how many recently opened pages the list returns — the suite's sidebar shows five.
const recentCount = 5

// CanView reports whether memberID may still view pageID. A list re-checks every page with it, so a
// pin on a page whose access was since withdrawn is not shown (its title would leak).
type CanView func(ctx context.Context, memberID, pageID string) bool

type Handler struct {
	store   *Store
	pageEnf *permission.Enforcer
	canView CanView
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the page enforcer (pin/unpin need View on the page) and the per-page check the
// lists filter through. Without them the pin routes fail closed (a nil Enforcer denies with 404) and
// the lists show nothing.
func (h *Handler) WithAccess(pageEnf *permission.Enforcer, canView CanView) *Handler {
	h.pageEnf = pageEnf
	h.canView = canView
	return h
}

func (h *Handler) Mount(r chi.Router) {
	r.With(h.pageEnf.Require(permission.AccessView)).Put("/spaces/{spaceID}/pages/{pageID}/pin", h.Pin)
	r.With(h.pageEnf.Require(permission.AccessView)).Delete("/spaces/{spaceID}/pages/{pageID}/pin", h.Unpin)
	r.Get("/workspaces/{wsID}/pins", h.ListPins)
	r.Get("/workspaces/{wsID}/recent-pages", h.ListRecent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// actor is the member and workspace RequireAccess resolved for this page from the verified
// identity — nothing is read from the request body.
func actor(r *http.Request) (wsID, memberID string, ok bool) {
	m, okM := permission.ActorFromContext(r.Context())
	ws, okW := permission.WorkspaceFromContext(r.Context())
	return ws, m, okM && okW && m != "" && ws != ""
}

func (h *Handler) Pin(w http.ResponseWriter, r *http.Request) {
	ws, member, ok := actor(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	if err := h.store.Pin(r.Context(), ws, member, chi.URLParam(r, "pageID")); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not pin the page")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"pinned": true})
}

func (h *Handler) Unpin(w http.ResponseWriter, r *http.Request) {
	ws, member, ok := actor(r)
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	if err := h.store.Unpin(r.Context(), ws, member, chi.URLParam(r, "pageID")); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not unpin the page")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"pinned": false})
}

func (h *Handler) ListPins(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, func(ctx context.Context, ws, member string) ([]Entry, error) { return h.store.Pins(ctx, ws, member) })
}

func (h *Handler) ListRecent(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, func(ctx context.Context, ws, member string) ([]Entry, error) {
		return h.store.Recent(ctx, ws, member, recentCount)
	})
}

// list answers "mine, here": {wsID} is authorized against the verified memberships, the member is
// the caller's id in that workspace, and every page is re-checked for View before it is listed.
func (h *Handler) list(w http.ResponseWriter, r *http.Request, read func(context.Context, string, string) ([]Entry, error)) {
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized on the next line before any store op
	m, ok := authz.AuthorizeWorkspace(r.Context(), wsID)
	if !ok {
		writeErr(w, http.StatusForbidden, "not a member of this workspace")
		return
	}
	entries, err := read(r.Context(), wsID, m.MemberID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the list")
		return
	}
	out := []Entry{}
	for _, e := range entries {
		if h.canView != nil && h.canView(r.Context(), m.MemberID, e.PageID) {
			out = append(out, e)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
