package database

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
	pageEnf *permission.Enforcer // A3: by-page access for the page-scoped create route
	dbEnf   *permission.Enforcer // A3: by-database access for the /databases/{dbID}/* routes (resolves db→page)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcers: pageEnf gates the page-scoped create route, dbEnf gates
// every /databases/{dbID}/* route (its resolver reads dbID from the URL and inherits the owning page's
// access). Without them the routes mount unguarded (tests). A nil enforcer FAILS CLOSED (404).
func (h *Handler) WithAccess(pageEnf, dbEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	h.dbEnf = dbEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// A3: page-scoped create is gated by page access (Edit). The /databases/{dbID}/* routes are gated
	// by dbEnf, whose resolver reads {dbID} from the URL and inherits the OWNING PAGE's access
	// (databases.page_id → pages) — an inline database is page content, so writing it is an Edit-tier
	// action and reading it a View-tier action, exactly like blocks. Before dbEnf they were gated by
	// workspace MEMBERSHIP only (SEC-4 L2), so a view-only member could mutate schema/rows/views.
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/pages/{pageID}/databases", h.CreateDatabase)
	r.With(h.dbEnf.Require(permission.AccessView)).Get("/databases/{dbID}", h.GetDatabase)
	r.With(h.dbEnf.Require(permission.AccessEdit)).Patch("/databases/{dbID}/schema", h.UpdateSchema)

	r.With(h.dbEnf.Require(permission.AccessEdit)).Post("/databases/{dbID}/rows", h.CreateRow)
	r.With(h.dbEnf.Require(permission.AccessView)).Get("/databases/{dbID}/rows", h.ListRows)
	r.With(h.dbEnf.Require(permission.AccessEdit)).Patch("/databases/{dbID}/rows/{rowID}", h.UpdateRow)
	r.With(h.dbEnf.Require(permission.AccessEdit)).Delete("/databases/{dbID}/rows/{rowID}", h.DeleteRow)

	r.With(h.dbEnf.Require(permission.AccessEdit)).Post("/databases/{dbID}/views", h.CreateView)
	r.With(h.dbEnf.Require(permission.AccessView)).Get("/databases/{dbID}/views", h.ListViews)
	r.With(h.dbEnf.Require(permission.AccessEdit)).Patch("/databases/{dbID}/views/{viewID}", h.UpdateView)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ─── Database ───

func (h *Handler) CreateDatabase(w http.ResponseWriter, r *http.Request) {
	var in Database
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	in.PageID = chi.URLParam(r, "pageID")
	// SEC: WorkspaceOrEmpty no-ops for a multi-workspace caller, leaving the BODY's
	// workspace_id as the new database's tenancy key. Derive it from the parent page.
	ws, ok := permission.WorkspaceFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "cannot resolve the workspace for this page")
		return
	}
	in.WorkspaceID = ws
	db, err := h.store.CreateDatabase(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, db)
}

func (h *Handler) GetDatabase(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	db, err := h.store.GetDatabase(r.Context(), chi.URLParam(r, "dbID"), wsIDs)
	if err != nil {
		writeErr(w, http.StatusNotFound, "database not found")
		return
	}
	writeJSON(w, http.StatusOK, db)
}

func (h *Handler) UpdateSchema(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Schema []ColumnDef `json:"schema"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	wsIDs := authz.WorkspaceIDs(r.Context())
	db, err := h.store.UpdateSchema(r.Context(), chi.URLParam(r, "dbID"), in.Schema, wsIDs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "database not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, db)
}

// ─── Rows ───

func (h *Handler) CreateRow(w http.ResponseWriter, r *http.Request) {
	var in Row
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	in.DatabaseID = chi.URLParam(r, "dbID")
	wsIDs := authz.WorkspaceIDs(r.Context())
	row, err := h.store.CreateRow(r.Context(), in, wsIDs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "database not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *Handler) UpdateRow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Values map[string]any `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	wsIDs := authz.WorkspaceIDs(r.Context())
	row, err := h.store.UpdateRow(r.Context(), chi.URLParam(r, "rowID"), in.Values, wsIDs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "row not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (h *Handler) DeleteRow(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	if err := h.store.DeleteRow(r.Context(), chi.URLParam(r, "rowID"), wsIDs); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "row not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) ListRows(w http.ResponseWriter, r *http.Request) {
	dbID := chi.URLParam(r, "dbID")
	wsIDs := authz.WorkspaceIDs(r.Context())
	var view *DatabaseView
	if viewID := r.URL.Query().Get("view_id"); viewID != "" {
		// Resolve the saved view so filters/sort are applied. A
		// missing view is silently ignored — the rows still come
		// back, just unfiltered.
		views, _ := h.store.ListViews(r.Context(), dbID, wsIDs)
		for i := range views {
			if views[i].ID == viewID {
				view = &views[i]
				break
			}
		}
	}
	rows, err := h.store.ListRows(r.Context(), dbID, view, wsIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []Row{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// ─── Views ───

func (h *Handler) CreateView(w http.ResponseWriter, r *http.Request) {
	var in DatabaseView
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	in.DatabaseID = chi.URLParam(r, "dbID")
	wsIDs := authz.WorkspaceIDs(r.Context())
	v, err := h.store.CreateView(r.Context(), in, wsIDs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "database not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (h *Handler) ListViews(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	views, err := h.store.ListViews(r.Context(), chi.URLParam(r, "dbID"), wsIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if views == nil {
		views = []DatabaseView{}
	}
	writeJSON(w, http.StatusOK, views)
}

func (h *Handler) UpdateView(w http.ResponseWriter, r *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	wsIDs := authz.WorkspaceIDs(r.Context())
	v, err := h.store.UpdateView(r.Context(), chi.URLParam(r, "viewID"), updates, wsIDs)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "view not found")
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}
