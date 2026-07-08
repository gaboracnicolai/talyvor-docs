package database

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
)

type Handler struct{ store *Store }

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

func (h *Handler) Mount(r chi.Router) {
	r.Post("/pages/{pageID}/databases", h.CreateDatabase)
	r.Get("/databases/{dbID}", h.GetDatabase)
	r.Patch("/databases/{dbID}/schema", h.UpdateSchema)

	r.Post("/databases/{dbID}/rows", h.CreateRow)
	r.Get("/databases/{dbID}/rows", h.ListRows)
	r.Patch("/databases/{dbID}/rows/{rowID}", h.UpdateRow)
	r.Delete("/databases/{dbID}/rows/{rowID}", h.DeleteRow)

	r.Post("/databases/{dbID}/views", h.CreateView)
	r.Get("/databases/{dbID}/views", h.ListViews)
	r.Patch("/databases/{dbID}/views/{viewID}", h.UpdateView)
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
	if ws := authz.WorkspaceOrEmpty(r.Context()); ws != "" {
		in.WorkspaceID = ws
	}
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
