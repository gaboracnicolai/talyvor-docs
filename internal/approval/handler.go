package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// emailer is the subset of the notification dispatcher this handler calls.
// Local interface so approval stays free of a notification import; email is
// optional and opt-in, and the call is best-effort (never fails the request).
type emailer interface {
	ApprovalRequested(ctx context.Context, pageID, workspaceID, requestedBy string, reviewers []string, message string)
}

type Handler struct {
	store   *Store
	emailer emailer
}

func NewHandler(store *Store) *Handler { return &Handler{store: store} }

// WithEmailer wires the email dispatcher. Optional/opt-in: without it, approval
// behaviour is unchanged.
func (h *Handler) WithEmailer(e emailer) *Handler {
	h.emailer = e
	return h
}

func (h *Handler) Mount(r chi.Router) {
	r.Post("/spaces/{spaceID}/pages/{pageID}/approval", h.Request)
	r.Get("/spaces/{spaceID}/pages/{pageID}/approval", h.Latest)
	r.Post("/spaces/{spaceID}/pages/{pageID}/approval/{requestID}/decide", h.Decide)
	r.Post("/spaces/{spaceID}/pages/{pageID}/publish", h.Publish)
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
	if in.WorkspaceID == "" {
		in.WorkspaceID = r.Header.Get("X-Talyvor-Workspace")
	}
	requestedBy := r.Header.Get("X-Member-Id")
	if requestedBy == "" {
		requestedBy = "user"
	}
	req, err := h.store.RequestApproval(r.Context(),
		pageID, in.WorkspaceID, requestedBy, in.Reviewers, in.Message, in.DueDate)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.emailer != nil {
		// Post-commit: the approval rows + page status are persisted.
		h.emailer.ApprovalRequested(r.Context(), req.PageID, req.WorkspaceID, req.RequestedBy, req.Reviewers, req.Message)
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
	reqs, err := h.store.ListByPage(r.Context(), pageID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := latestResponse{Decisions: []ReviewDecision{}}
	if len(reqs) > 0 {
		resp.Request = &reqs[0]
		decisions, err := h.store.GetDecisions(r.Context(), reqs[0].ID)
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
	reviewerID := r.Header.Get("X-Member-Id")
	if reviewerID == "" {
		writeErr(w, http.StatusBadRequest, "X-Member-Id header required")
		return
	}
	var in decideBody
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := h.store.Decide(r.Context(), requestID, reviewerID, in.Decision, in.Comment); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	if err := h.store.PublishApproved(r.Context(), chi.URLParam(r, "pageID")); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) Pending(w http.ResponseWriter, r *http.Request) {
	wsID := chi.URLParam(r, "wsID")
	reviewerID := r.URL.Query().Get("reviewer_id")
	if reviewerID == "" {
		reviewerID = r.Header.Get("X-Member-Id")
	}
	out, err := h.store.ListPending(r.Context(), reviewerID, wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []ApprovalRequest{}
	}
	writeJSON(w, http.StatusOK, out)
}
