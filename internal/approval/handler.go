package approval

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/permission"
)

type Handler struct {
	store   *Store
	pageEnf *permission.Enforcer // A3: approval acts on a page (nil = unguarded)
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithAccess wires the A3 page access enforcer. Without it the routes mount unguarded (tests).
func (h *Handler) WithAccess(pageEnf *permission.Enforcer) *Handler {
	h.pageEnf = pageEnf
	return h
}

func (h *Handler) Mount(r chi.Router) {
	// Requesting approval / publishing mutate the page → Edit; reading the status / deciding are
	// View (the Decide store already gates on the verified reviewer). Pending is workspace-level.
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/approval", h.Request)
	r.With(h.pageEnf.Require(permission.AccessView)).Get("/spaces/{spaceID}/pages/{pageID}/approval", h.Latest)
	r.With(h.pageEnf.Require(permission.AccessView)).Post("/spaces/{spaceID}/pages/{pageID}/approval/{requestID}/decide", h.Decide)
	r.With(h.pageEnf.Require(permission.AccessEdit)).Post("/spaces/{spaceID}/pages/{pageID}/publish", h.Publish)
	r.Get("/workspaces/{wsID}/approvals/pending", h.Pending)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type requestBody struct {
	Reviewers   []string   `json:"reviewers"`
	Message     string     `json:"message"`
	DueDate     *time.Time `json:"due_date"`
	WorkspaceID string     `json:"workspace_id"`
}

func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	var in requestBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if ws := authz.WorkspaceOrEmpty(r.Context()); ws != "" {
		in.WorkspaceID = ws
	}
	requestedBy := authz.ActorOrEmpty(r.Context())
	if requestedBy == "" {
		requestedBy = "user"
	}
	req, err := h.store.RequestApproval(r.Context(),
		pageID, in.WorkspaceID, requestedBy, in.Reviewers, in.Message, in.DueDate)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

// Latest returns the most recent approval request (if any) plus its
// decisions — the shape the frontend ApprovalPanel reads.
type latestResponse struct {
	Request   *ApprovalRequest `json:"request"`
	Decisions []ReviewDecision `json:"decisions"`
}

func (h *Handler) Latest(w http.ResponseWriter, r *http.Request) {
	pageID := chi.URLParam(r, "pageID")
	wsIDs := authz.WorkspaceIDs(r.Context())
	// SEC-4 L2: a foreign page id is 404 (no oracle), not an empty 200.
	if ok, err := h.store.PageInWorkspaces(r.Context(), pageID, wsIDs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	} else if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	reqs, err := h.store.ListByPage(r.Context(), pageID, wsIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := latestResponse{Decisions: []ReviewDecision{}}
	if len(reqs) > 0 {
		resp.Request = &reqs[0]
		decisions, err := h.store.GetDecisions(r.Context(), reqs[0].ID, wsIDs)
		if err == nil {
			resp.Decisions = decisions
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type decideBody struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
}

func (h *Handler) Decide(w http.ResponseWriter, r *http.Request) {
	requestID := chi.URLParam(r, "requestID")
	reviewerID := authz.ActorOrEmpty(r.Context())
	if reviewerID == "" {
		writeErr(w, http.StatusBadRequest, "verified identity required")
		return
	}
	var in decideBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	wsIDs := authz.WorkspaceIDs(r.Context())
	if err := h.store.Decide(r.Context(), requestID, reviewerID, in.Decision, in.Comment, wsIDs); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	if err := h.store.PublishApproved(r.Context(), chi.URLParam(r, "pageID"), wsIDs); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Pending(w http.ResponseWriter, r *http.Request) {
	wsIDs := authz.WorkspaceIDs(r.Context())
	reviewerID := r.URL.Query().Get("reviewer_id")
	if reviewerID == "" {
		reviewerID = authz.ActorOrEmpty(r.Context())
	}
	out, err := h.store.ListPending(r.Context(), reviewerID, wsIDs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []ApprovalRequest{}
	}
	writeJSON(w, http.StatusOK, out)
}
