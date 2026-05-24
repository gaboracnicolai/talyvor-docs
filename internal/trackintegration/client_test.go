package trackintegration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// httpFixture spins up a stub Track server so the client can exercise
// the real fetch path without us mocking *http.Client. The mux maps
// URL prefixes to handlers; unmatched paths 404.
func httpFixture(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ─── IsConfigured ──────────────────────────────────────────

func TestIsConfigured_BothFieldsRequired(t *testing.T) {
	if New("", "").IsConfigured() {
		t.Error("empty URL should be unconfigured")
	}
	if New("http://x", "").IsConfigured() {
		t.Error("empty API key should be unconfigured")
	}
	if !New("http://x", "k").IsConfigured() {
		t.Error("both set should be configured")
	}
}

// ─── GetIssue ──────────────────────────────────────────────

func TestGetIssue_ReturnsIssueRef(t *testing.T) {
	srv := httpFixture(t, map[string]http.HandlerFunc{
		"/v1/workspaces/ws-1/issues/i-1": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          "i-1",
				"identifier":  "ENG-42",
				"title":       "Add dark mode",
				"status":      "in_progress",
				"priority":    2,
				"ai_cost_usd": 12.34,
			})
		},
	})

	c := New(srv.URL, "test-key")
	out, err := c.GetIssue(context.Background(), "ws-1", "i-1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if out.Identifier != "ENG-42" {
		t.Errorf("identifier = %q", out.Identifier)
	}
	if out.AICostUSD != 12.34 {
		t.Errorf("ai_cost_usd = %v", out.AICostUSD)
	}
}

func TestGetIssue_ReturnsNilWhenUnconfigured(t *testing.T) {
	c := New("", "")
	out, err := c.GetIssue(context.Background(), "ws", "i")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil ref when Track unconfigured, got %+v", out)
	}
}

func TestGetIssue_GracefulOnNetworkFailure(t *testing.T) {
	// Point at a guaranteed-dead port. The client should NOT bubble
	// the connection refused upstream — Track integration is
	// optional; embeds should fall back to "issue unavailable" UX
	// rather than 500ing the docs API.
	c := New("http://127.0.0.1:1", "test-key")
	out, _ := c.GetIssue(context.Background(), "ws", "i")
	if out != nil {
		t.Errorf("expected nil on unreachable Track, got %+v", out)
	}
}

func TestGetIssue_CachesFor30Seconds(t *testing.T) {
	calls := 0
	srv := httpFixture(t, map[string]http.HandlerFunc{
		"/v1/workspaces/ws/issues/i": func(w http.ResponseWriter, _ *http.Request) {
			calls++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "i", "identifier": "T-1"})
		},
	})
	c := New(srv.URL, "k")
	if _, err := c.GetIssue(context.Background(), "ws", "i"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := c.GetIssue(context.Background(), "ws", "i"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 upstream call (cache hit), got %d", calls)
	}
}

// ─── SearchIssues ──────────────────────────────────────────

func TestSearchIssues_ReturnsResults(t *testing.T) {
	srv := httpFixture(t, map[string]http.HandlerFunc{
		"/v1/workspaces/ws/issues/search": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("q") != "dark mode" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "i-1", "identifier": "ENG-1", "title": "Dark mode"},
				{"id": "i-2", "identifier": "ENG-2", "title": "Dark mode follow-up"},
			})
		},
	})
	c := New(srv.URL, "k")
	out, err := c.SearchIssues(context.Background(), "ws", "dark mode")
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("got %d, want 2", len(out))
	}
}

func TestSearchIssues_UnconfiguredReturnsEmpty(t *testing.T) {
	c := New("", "")
	out, err := c.SearchIssues(context.Background(), "ws", "q")
	if err != nil {
		t.Fatalf("SearchIssues: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty, got %+v", out)
	}
}

// ─── GetPageBacklinks ──────────────────────────────────────

func TestGetPageBacklinks_StubReturnsEmpty(t *testing.T) {
	// Phase 4: Track doesn't have the linked_doc filter yet. The
	// client implements the endpoint as a stub that returns []
	// gracefully so the docs UI can render the "no backlinks yet"
	// state without erroring.
	c := New("http://example", "k")
	out, err := c.GetPageBacklinks(context.Background(), "ws", "p-1")
	if err != nil {
		t.Fatalf("GetPageBacklinks: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty stub result, got %+v", out)
	}
}
