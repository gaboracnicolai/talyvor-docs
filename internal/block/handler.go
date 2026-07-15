package block

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/model"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store *Store
	// A3: blocks are the page's content. The /pages/{pageID}/blocks routes resolve access from the
	// pageID (pageEnf); the /blocks/{blockID} routes resolve the owning page from the block (blockEnf).
	pageEnf  *permission.Enforcer
	blockEnf *permission.Enforcer
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 access enforcers. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf, blockEnf *permission.Enforcer) *Handler {
	h.pageEnf, h.blockEnf = pageEnf, blockEnf
	return h
}

// Mount registers the block CRUD endpoints. Phase 1 keeps these
// minimal — the collaborative editor that drives heavy per-block
// traffic ships in Phase 2.
func (h *Handler) Mount(r chi.Router) {
	r.Route("/pages/{pageID}/blocks", func(r chi.Router) {
		// Blocks ARE the page's content: read=View, mutation=Edit.
		r.With(h.pageEnf.Require(permission.AccessView)).Get("/", h.List)
		r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/", h.Create)
	})
	r.With(h.blockEnf.Require(permission.AccessEdit)).Patch("/blocks/{blockID}", h.Update)
	r.With(h.blockEnf.Require(permission.AccessEdit)).Delete("/blocks/{blockID}", h.Delete)
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

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	out, err := h.store.ListByPage(r.Context(), chi.URLParam(r, "pageID"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "LIST_FAILED", err.Error())
		return
	}
	if out == nil {
		out = []model.Block{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in model.Block
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	in.PageID = chi.URLParam(r, "pageID")
	// nosemgrep: docs-no-body-supplied-authority -- model.Block carries NO authority field: it is {ID, PageID, Type, Content, Position, ParentID, CreatedAt, UpdatedAt} — no workspace_id and no created_by, so there is nothing here for the body to forge. Tenancy comes from the parent page, which blockEnf/pageEnf authorized upstream (cmd/docs/main.go), and blocks are reached only via page_id. The rule is a shape rule and cannot see the struct's fields. IF model.Block EVER GAINS a workspace_id/created_by/*_by FIELD, DELETE THIS SUPPRESSION — the finding becomes real that day.
	out, err := h.store.Create(r.Context(), in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "CREATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content  string  `json:"content"`
		Position float64 `json:"position"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	out, err := h.store.Update(r.Context(), chi.URLParam(r, "blockID"), in.Content, in.Position)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.store.Delete(r.Context(), chi.URLParam(r, "blockID")); err != nil {
		writeErr(w, http.StatusInternalServerError, "DELETE_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
