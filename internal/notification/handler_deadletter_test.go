package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

type fakeLister struct{ rows []DeadLetter }

func (f fakeLister) List(_ context.Context, _ int) ([]DeadLetter, error) { return f.rows, nil }

func TestHandler_ListDeadLetters(t *testing.T) {
	h := NewHandler().WithDeadLetters(fakeLister{rows: []DeadLetter{
		{ID: 1, Subject: "Review requested: Spec", Attempts: 3, LastError: "smtp down", Recipients: []string{"a@b.c"}},
	}})
	r := chi.NewRouter()
	h.Mount(r)

	req := httptest.NewRequest("GET", "/notifications/dead-letters", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var out []DeadLetter
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Subject != "Review requested: Spec" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandler_ListDeadLetters_NotConfiguredReturnsEmpty(t *testing.T) {
	h := NewHandler() // no dead-letter lister wired
	r := chi.NewRouter()
	h.Mount(r)

	req := httptest.NewRequest("GET", "/notifications/dead-letters", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "[]\n" && got != "[]" {
		t.Fatalf("want empty JSON array, got %q", got)
	}
}
