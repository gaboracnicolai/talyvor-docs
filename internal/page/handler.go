package page

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/metrics"
	"github.com/talyvor/docs/internal/model"
)

// emailer is the subset of the notification dispatcher this handler calls when
// a page body is created/edited (to notify @mentioned users). Local interface;
// optional/opt-in and best-effort.
type emailer interface {
	PageMentioned(ctx context.Context, pageID, text, actorID string)
}

// Comments moved to internal/comment in the threaded-comments
// rework. The page handler retains the constructor signature for
// backwards compatibility — pool is no longer used here but main
// still hands it in for symmetry with other handlers.
type Handler struct {
	store   *Store
	emailer emailer
}

func NewHandler(store *Store, _ *pgxpool.Pool) *Handler {
	return &Handler{store: store}
}

// WithEmailer wires the email dispatcher. Optional/opt-in.
func (h *Handler) WithEmailer(e emailer) *Handler {
	h.emailer = e
	return h
}

// Mount registers every page-scoped route under /v1. Comments,
// versions, view, verify, search, and stale all live under the
// same handler so the page surface is one chi sub-router.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/spaces/{spaceID}/pages", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{pageID}", h.Get)
		r.Patch("/{pageID}", h.Update)
		r.Delete("/{pageID}", h.Delete)

		r.Post("/{pageID}/view", h.RecordView)
		r.Post("/{pageID}/verify", h.Verify)

		r.Get("/{pageID}/versions", h.GetVersions)
		r.Post("/{pageID}/versions/{version}/restore", h.RestoreVersion)

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
	if h.emailer != nil {
		// Mentions in the new page body. Actor = creator (excluded inside).
		h.emailer.PageMentioned(r.Context(), out.ID, out.ContentText, out.CreatedBy)
	}
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
	out, err := h.store.GetByID(r.Context(), chi.URLParam(r, "pageID"))
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
	// Propagate the caller's member identity for the lock guard.
	if _, set := updates["updated_by"]; !set {
		if v := r.Header.Get("X-Member-Id"); v != "" {
			updates["updated_by"] = v
		}
	}
	out, err := h.store.Update(r.Context(), chi.URLParam(r, "pageID"), updates)
	if err != nil {
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
	if h.emailer != nil {
		// Only scan for mentions when the body actually changed.
		if _, ok := updates["content"]; ok {
			actor, _ := updates["updated_by"].(string)
			h.emailer.PageMentioned(r.Context(), out.ID, out.ContentText, actor)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Delete(r.Context(), chi.URLParam(r, "pageID")); err != nil {
		writeErr(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── view / verify ────────────────────────────────────────

func (h *Handler) RecordView(w http.ResponseWriter, r *http.Request) {
	if err := h.store.RecordView(r.Context(),
		chi.URLParam(r, "pageID"), r.Header.Get("X-Member-Id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "VIEW_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Verify(r.Context(),
		chi.URLParam(r, "pageID"), r.Header.Get("X-Member-Id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "VERIFY_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ─── versions ─────────────────────────────────────────────

func (h *Handler) GetVersions(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.GetVersions(r.Context(), chi.URLParam(r, "pageID"))
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
	out, err := h.store.RestoreVersion(r.Context(), chi.URLParam(r, "pageID"), n)
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
