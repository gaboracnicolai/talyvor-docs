package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// deadLetterLister is the read side of the dead-letter surface. *DeadLetterStore
// satisfies it; the handler keeps it as an interface so the route can be tested
// without a database.
type deadLetterLister interface {
	List(ctx context.Context, limit int) ([]DeadLetter, error)
}

// Handler exposes the read-only admin surface for the notification system.
// Today that is the email dead-letter list; Docs notifications themselves are
// dispatched from the page/approval/comment/freshness seams, not served here.
type Handler struct {
	deadLetters deadLetterLister
}

func NewHandler() *Handler { return &Handler{} }

// WithDeadLetters wires the dead-letter read surface. Optional: without it, the
// route returns an empty list.
func (h *Handler) WithDeadLetters(l deadLetterLister) *Handler {
	h.deadLetters = l
	return h
}

func (h *Handler) Mount(r chi.Router) {
	r.Route("/notifications", func(r chi.Router) {
		// Admin: messages the email queue permanently failed to deliver.
		r.Get("/dead-letters", h.ListDeadLetters)
	})
}

// ListDeadLetters returns the most recent permanently-failed email deliveries.
// Returns an empty array when email/dead-letter is not configured (nothing
// sends, so nothing can fail).
func (h *Handler) ListDeadLetters(w http.ResponseWriter, r *http.Request) {
	out := []DeadLetter{}
	if h.deadLetters != nil {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		rows, err := h.deadLetters.List(r.Context(), limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error(), "code": "DLQ_LIST_FAILED"})
			return
		}
		if rows != nil {
			out = rows
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
