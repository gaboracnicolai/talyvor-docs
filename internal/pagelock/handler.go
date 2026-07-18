package pagelock

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
	pageEnf *permission.Enforcer // A3: by-page access (view/edit)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// Lock/Unlock mutate the page's edit state → Edit; reading lock state → View.
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/lock", h.Lock)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Delete("/spaces/{spaceID}/pages/{pageID}/lock", h.Unlock)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/lock", h.Get)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// These routes take NO authority from the request body. Both member_id and is_admin
// used to live here and both were forgeable:
//
//   - is_admin let any Edit-tier member bypass "only the locker can unlock" (Run 1).
//   - member_id NAMED THE ACTOR whenever authz.ActorOrEmpty was empty — which it was for
//     every caller with != 1 memberships — so a two-workspace member unlocked another
//     member's lock by sending {"member_id": "<locker>"}. The same caller could not use
//     the feature honestly: with no verified actor, an empty body was rejected outright.
//
// Both now come from the verified identity: actorFor() below, and
// permission.IsAdminFromContext in Unlock. The request body is not read at all — an
// endpoint that takes no authority from the client cannot be lied to.

// actorFor resolves the acting member from the GATEWAY-VERIFIED identity, in the
// workspace that owns this page. RequireAccess (which gates every route in this package's
// Mount) resolves it via authz.MemberIDForWorkspace and stashes it — correct for ANY
// membership count, so there is no fallback and nothing to forge. Empty only on an
// unguarded mount, which fails closed at the call sites below.
func actorFor(r *http.Request) string {
	m, _ := permission.ActorFromContext(r.Context())
	return m
}

func (h *Handler) Lock(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	memberID := actorFor(r)
	if memberID == "" {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	state, err := h.store.LockInWorkspaces(r.Context(), pageID, memberID, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		// Lock conflicts map to 423 (Locked) so the frontend can
		// surface a specific "already locked" affordance vs a
		// generic failure.
		writeErr(w, http.StatusLocked, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (h *Handler) Unlock(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	memberID := actorFor(r)
	if memberID == "" {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return
	}
	// Admin status comes from the VERIFIED identity resolved against the permission
	// model by the same RequireAccess middleware that gates this route — never from the
	// request body. Fails closed: an unguarded mount yields no level in context, so
	// isAdmin is false and only the locker can unlock.
	isAdmin := permission.IsAdminFromContext(r.Context())
	err := h.store.UnlockInWorkspaces(r.Context(), pageID, memberID, isAdmin, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	state, err := h.store.GetLockState(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, state)
}
