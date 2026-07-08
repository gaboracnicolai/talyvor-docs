package page

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/metrics"
	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/permission"
)

// Comments moved to internal/comment in the threaded-comments
// rework. The page handler retains the constructor signature for
// backwards compatibility — pool is no longer used here but main
// still hands it in for symmetry with other handlers.
type Handler struct {
	store    *Store
	pageEnf  *permission.Enforcer // A3: by-page access (view/edit)
	spaceEnf *permission.Enforcer // A3: by-space access for the space-scoped create/list routes
}

func NewHandler(store *Store, _ *pgxpool.Pool) *Handler {
	return &Handler{store: store}
}

// WithAccess wires the A3 access enforcers. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf, spaceEnf *permission.Enforcer) *Handler {
	h.pageEnf, h.spaceEnf = pageEnf, spaceEnf
	return h
}

// Mount registers every page-scoped route under /v1. Comments,
// versions, view, verify, search, and stale all live under the
// same handler so the page surface is one chi sub-router.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/spaces/{spaceID}/pages", func(r chi.Router) {
		// Create a page in / list a space's pages → space-level (Edit to add, View to list).
		r.With(h.spaceEnf.Require(permission.AccessEdit)).Post("/", h.Create)
		r.With(h.spaceEnf.Require(permission.AccessView)).Get("/", h.List)
		// Per-page: read=View, content mutation=Edit.
		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}", h.Get)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Patch("/{pageID}", h.Update)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Delete("/{pageID}", h.Delete)

		r.With(h.pageEnf.Require(permission.AccessView)).Post("/{pageID}/view", h.RecordView)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/{pageID}/verify", h.Verify)

		r.With(h.pageEnf.Require(permission.AccessView)).Get("/{pageID}/versions", h.GetVersions)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/{pageID}/versions/{version}/restore", h.RestoreVersion)

		// Comment routes live in internal/comment as of the threaded-
		// comments rework. The legacy handlers below stay for the
		// /pages/{pageID}/comments/{commentID} update path, but the
		// list/create/resolve trio is owned by the new package.
	})

	r.Get("/workspaces/{wsID}/pages/search", h.Search)
	r.Get("/workspaces/{wsID}/pages/stale", h.Stale)
}

type apiError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Error: msg, Code: code})
}

// ─── page CRUD ────────────────────────────────────────────

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "spaceID")
	var in model.Page
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	in.SpaceID = spaceID
	out, err := h.store.Create(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	metrics.PagesCreated.WithLabelValues(out.SpaceID).Inc()
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	spaceID := chi.URLParam(r, "spaceID")
	filter := PageFilter{SpaceID: spaceID}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	out, err := h.store.List(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Page{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	// SEC-4: scope to the caller's verified workspace membership — a page in another
	// workspace is not-found (404), never leaked.
	out, err := h.store.GetByIDInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if err != nil {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	// SEC-4: the editor identity for the lock guard is the VERIFIED member id, never a
	// client-supplied header or body field. Overwrite any caller-provided updated_by.
	if mid, ok := authz.SingleMemberID(r.Context()); ok {
		updates["updated_by"] = mid
	} else {
		delete(updates, "updated_by")
	}
	out, err := h.store.UpdateInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), updates, authz.WorkspaceIDs(r.Context()))
	if err != nil {
		// Cross-tenant / unknown page → 404 (never leak existence).
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		// 423 Locked is the precise signal for lock conflicts so the
		// frontend can render a specific banner; everything else is
		// a generic 400 because it's caller-supplied bad input.
		if errors.Is(err, ErrLocked) {
			writeErr(w, http.StatusLocked, "LOCKED", err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	err := h.store.DeleteInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── view / verify ────────────────────────────────────────

func (h *Handler) RecordView(w http.ResponseWriter, r *http.Request) {
	viewer, _ := authz.SingleMemberID(r.Context()) // verified actor, never X-Member-Id
	err := h.store.RecordViewInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), viewer, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VIEW_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	verifier, _ := authz.SingleMemberID(r.Context()) // verified actor, never X-Member-Id
	err := h.store.VerifyInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), verifier, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VERIFY_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── versions ─────────────────────────────────────────────

func (h *Handler) GetVersions(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.GetVersionsInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "VERSIONS_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.PageVersion{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) RestoreVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_VERSION", "version must be int")
		return
	}
	out, err := h.store.RestoreVersionInWorkspaces(r.Context(), chi.URLParam(r, "pageID"), n, authz.WorkspaceIDs(r.Context()))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "RESTORE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── search + stale ───────────────────────────────────────

func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	q := r.URL.Query().Get("q")
	if q == "" {
		writeErr(w, http.StatusBadRequest, "BAD_PARAMS", "q required")
		return
	}
	limit := 25
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	out, err := h.store.Search(r.Context(), wsID, q, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "SEARCH_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Page{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Stale(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.GetStalePages(r.Context(), chi.URLParam(r, "wsID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "STALE_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Page{}
	}
	writeJSON(w, http.StatusOK, out)
}
