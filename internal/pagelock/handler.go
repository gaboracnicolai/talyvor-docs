package pagelock

import (
	"encoding/json"
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

type lockBody struct {
	MemberID string `json:"member_id"`
}

// unlockBody no longer carries is_admin. It used to, and the store trusted it to
// bypass the "only the locker can unlock" rule — so any Edit-tier member could steal
// another member's lock with {"is_admin": true}, while a REAL admin who sent no claim
// was denied. Admin status is now resolved from the gateway-verified identity against
// the permission model (permission.IsAdminFromContext). A client that still sends
// is_admin is simply ignored: the field is gone, and encoding/json drops unknown keys.
type unlockBody struct {
	MemberID string `json:"member_id"`
}

func memberFromReq(r *http.Request, fallback string) string {
	if v := authz.ActorOrEmpty(r.Context()); v != "" {
		return v
	}
	return fallback
}

func (h *Handler) Lock(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	var in lockBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		// Body is optional — fall back to the header.
		in = lockBody{}
	}
	memberID := memberFromReq(r, in.MemberID)
	if memberID == "" {
		writeErr(w, http.StatusBadRequest, "member_id required")
		return
	}
	state, err := h.store.Lock(r.Context(), pageID, memberID)
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
	var in unlockBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		in = unlockBody{}
	}
	memberID := memberFromReq(r, in.MemberID)
	if memberID == "" {
		writeErr(w, http.StatusBadRequest, "member_id required")
		return
	}
	// Admin status comes from the VERIFIED identity resolved against the permission
	// model by the same RequireAccess middleware that gates this route — never from the
	// request body. Fails closed: an unguarded mount yields no level in context, so
	// isAdmin is false and only the locker can unlock.
	isAdmin := permission.IsAdminFromContext(r.Context())
	if err := h.store.Unlock(r.Context(), pageID, memberID, isAdmin); err != nil {
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
