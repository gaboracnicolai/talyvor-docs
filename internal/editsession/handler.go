package editsession

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

// sessionStore is the handler's view of the store — the workspace-scoped ops. Narrow so the
// handler can't reach the page-scoped CanEdit guard adapter (that is store.Update's, not HTTP's).
type sessionStore interface {
	Acquire(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error)
	Heartbeat(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error)
	Release(ctx context.Context, pageID string, wsIDs []string, holder string) error
	Takeover(ctx context.Context, pageID string, wsIDs []string, holder string) (*Session, error)
	Get(ctx context.Context, pageID string, wsIDs []string) (*Session, error)
}

type Handler struct {
	store   sessionStore
	pageEnf *permission.Enforcer
}

func NewHandler(store sessionStore) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcer. Without it the routes mount unguarded (tests only).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	base := "/spaces/{spaceID}/pages/{pageID}/edit-session"
	// Acquiring / heartbeating / releasing / taking over the writer slot mutate edit state → Edit.
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post(base, h.Acquire)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post(base+"/heartbeat", h.Heartbeat)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post(base+"/takeover", h.Takeover)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Delete(base, h.Release)
	// Observing who holds the slot → View.
	r.With(h.pageEnf.Require(permission.AccessView)).Get(base, h.Get)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// actorFor resolves the acting member from the GATEWAY-VERIFIED identity in the page's
// workspace, exactly as internal/pagelock does. The request body is never read for authority.
func actorFor(r *http.Request) string {
	m, _ := permission.ActorFromContext(r.Context())
	return m
}

// wsScope is the SERVER-authorized workspace set. Never a client field.
func wsScope(r *http.Request) []string { return authz.WorkspaceIDs(r.Context()) }

// respond maps a claim result to HTTP. A live foreign session is 423 Locked (so the UI can show
// "<holder> is editing" and offer takeover); a cross-tenant/missing page is 404.
func (h *Handler) respondClaim(w http.ResponseWriter, sess *Session, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrHeldByOther):
		writeJSON(w, http.StatusLocked, map[string]any{"error": err.Error(), "session": sess})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, sess)
	}
}

func (h *Handler) requireActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	m := actorFor(r)
	if m == "" {
		writeErr(w, http.StatusForbidden, "cannot resolve the acting member for this page")
		return "", false
	}
	return m, true
}

func (h *Handler) Acquire(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	sess, err := h.store.Acquire(r.Context(), chi.URLParam(r, "pageID"), wsScope(r), member)
	h.respondClaim(w, sess, err)
}

func (h *Handler) Takeover(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	sess, err := h.store.Takeover(r.Context(), chi.URLParam(r, "pageID"), wsScope(r), member)
	h.respondClaim(w, sess, err)
}

func (h *Handler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	sess, err := h.store.Heartbeat(r.Context(), chi.URLParam(r, "pageID"), wsScope(r), member)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrHeldByOther):
		// The caller lost the slot (taken over / released). 409 so the client re-acquires.
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, sess)
	}
}

func (h *Handler) Release(w http.ResponseWriter, r *http.Request) {
	member, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	err := h.store.Release(r.Context(), chi.URLParam(r, "pageID"), wsScope(r), member)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	sess, err := h.store.Get(r.Context(), chi.URLParam(r, "pageID"), wsScope(r))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// No active session is a valid state — return an explicit "no holder" body, not 404.
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": nil})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}
