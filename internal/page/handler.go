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

// commentStore is the tiny subset of comment operations the page
// handler needs. We back it with a thin pool-backed implementation
// in this file so we don't need a separate comment package in
// Phase 1.
type commentStore interface {
	Create(ctx context.Context, c model.Comment) (*model.Comment, error)
	ListByPage(ctx context.Context, pageID string) ([]model.Comment, error)
	Update(ctx context.Context, id, content string) (*model.Comment, error)
	Resolve(ctx context.Context, id, resolverID string) error
}

type Handler struct {
	store   *Store
	comment commentStore
}

func NewHandler(store *Store, pool *pgxpool.Pool) *Handler {
	return &Handler{store: store, comment: newCommentStore(pool)}
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

		r.Get("/{pageID}/comments", h.ListComments)
		r.Post("/{pageID}/comments", h.CreateComment)
		r.Patch("/{pageID}/comments/{commentID}", h.UpdateComment)
		r.Post("/{pageID}/comments/{commentID}/resolve", h.ResolveComment)
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

// ─── comments ─────────────────────────────────────────────

func (h *Handler) ListComments(w http.ResponseWriter, r *http.Request) {
	out, err := h.comment.ListByPage(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Comment{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) CreateComment(w http.ResponseWriter, r *http.Request) {
	var in model.Comment
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	in.PageID = chi.URLParam(r, "pageID")
	if in.AuthorID == "" {
		in.AuthorID = r.Header.Get("X-Member-Id")
	}
	if in.Content == "" {
		writeErr(w, http.StatusBadRequest, "BAD_PARAMS", "content required")
		return
	}
	out, err := h.comment.Create(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) UpdateComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	out, err := h.comment.Update(r.Context(), chi.URLParam(r, "commentID"), in.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) ResolveComment(w http.ResponseWriter, r *http.Request) {
	if err := h.comment.Resolve(r.Context(),
		chi.URLParam(r, "commentID"), r.Header.Get("X-Member-Id")); err != nil {
		writeErr(w, http.StatusInternalServerError, "RESOLVE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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

// ─── inline comment store ─────────────────────────────────

// The comment surface is small enough to live alongside the page
// handler in Phase 1. A future phase can promote it to its own
// package when comment-specific features (notifications, mentions)
// land.

type pgxPool interface {
	commentStore
}

type defaultCommentStore struct{ pool *pgxpool.Pool }

func newCommentStore(pool *pgxpool.Pool) commentStore {
	if pool == nil {
		return nopComments{}
	}
	return &defaultCommentStore{pool: pool}
}

func (s *defaultCommentStore) Create(ctx context.Context, c model.Comment) (*model.Comment, error) {
	var out model.Comment
	err := s.pool.QueryRow(ctx,
		`INSERT INTO page_comments (page_id, block_id, author_id, content)
        VALUES ($1, $2, $3, $4)
        RETURNING id, page_id, block_id, author_id, content, resolved, resolved_by, created_at, updated_at`,
		c.PageID, c.BlockID, c.AuthorID, c.Content,
	).Scan(&out.ID, &out.PageID, &out.BlockID, &out.AuthorID, &out.Content,
		&out.Resolved, &out.ResolvedBy, &out.CreatedAt, &out.UpdatedAt)
	return &out, err
}

func (s *defaultCommentStore) ListByPage(ctx context.Context, pageID string) ([]model.Comment, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, page_id, block_id, author_id, content, resolved, resolved_by, created_at, updated_at
        FROM page_comments WHERE page_id = $1 ORDER BY created_at ASC`,
		pageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Comment
	for rows.Next() {
		var c model.Comment
		if err := rows.Scan(&c.ID, &c.PageID, &c.BlockID, &c.AuthorID, &c.Content,
			&c.Resolved, &c.ResolvedBy, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *defaultCommentStore) Update(ctx context.Context, id, content string) (*model.Comment, error) {
	var out model.Comment
	err := s.pool.QueryRow(ctx,
		`UPDATE page_comments SET content = $1, updated_at = NOW() WHERE id = $2
        RETURNING id, page_id, block_id, author_id, content, resolved, resolved_by, created_at, updated_at`,
		content, id,
	).Scan(&out.ID, &out.PageID, &out.BlockID, &out.AuthorID, &out.Content,
		&out.Resolved, &out.ResolvedBy, &out.CreatedAt, &out.UpdatedAt)
	return &out, err
}

func (s *defaultCommentStore) Resolve(ctx context.Context, id, resolverID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE page_comments SET resolved = true, resolved_by = $1, updated_at = NOW() WHERE id = $2`,
		resolverID, id,
	)
	return err
}

// nopComments stands in when the page handler is wired without a
// real pool (tests, dry-run mode). All operations no-op gracefully.
type nopComments struct{}

func (nopComments) Create(_ context.Context, _ model.Comment) (*model.Comment, error) {
	return nil, errors.New("comments: store not configured")
}
func (nopComments) ListByPage(_ context.Context, _ string) ([]model.Comment, error) { return nil, nil }
func (nopComments) Update(_ context.Context, _, _ string) (*model.Comment, error) {
	return nil, errors.New("comments: store not configured")
}
func (nopComments) Resolve(_ context.Context, _, _ string) error {
	return errors.New("comments: store not configured")
}

// silence unused-import lint for the `pgxPool` placeholder above.
var _ pgxPool = (*defaultCommentStore)(nil)
