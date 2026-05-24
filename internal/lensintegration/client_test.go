package lensintegration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeLens stands in for the Lens proxy. It records the last request
// so the tests can assert header + body shape.
type fakeLens struct {
	*httptest.Server
	lastPath    string
	lastHeaders http.Header
	lastBody    []byte
	respond     func(w http.ResponseWriter, r *http.Request)
}

func newFakeLens() *fakeLens {
	f := &fakeLens{}
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hello world"}]}`))
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastPath = r.URL.Path
		f.lastHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		f.lastBody = b
		f.respond(w, r)
	}))
	return f
}

func TestComplete_CallsAnthropicProxyPath(t *testing.T) {
	srv := newFakeLens()
	defer srv.Close()
	c := New(srv.URL, "k1")

	out, err := c.Complete(context.Background(), "ws-1", "Hello", "You are helpful", "claude-haiku-4-6")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if out != "hello world" {
		t.Fatalf("want 'hello world', got %q", out)
	}
	if srv.lastPath != "/v1/proxy/anthropic/v1/messages" {
		t.Fatalf("wrong proxy path: %s", srv.lastPath)
	}
	if got := srv.lastHeaders.Get("Authorization"); got != "Bearer k1" {
		t.Fatalf("missing auth header: %q", got)
	}
	if got := srv.lastHeaders.Get("X-Talyvor-Feature"); got != "docs-ai" {
		t.Fatalf("missing feature header: %q", got)
	}
	if got := srv.lastHeaders.Get("X-Talyvor-Workspace"); got != "ws-1" {
		t.Fatalf("missing workspace header: %q", got)
	}
}

func TestComplete_OpenAIProxyPath(t *testing.T) {
	srv := newFakeLens()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"openai reply"}}]}`))
	}
	defer srv.Close()
	c := New(srv.URL, "k1")

	out, err := c.CompleteOpenAI(context.Background(), "ws-1", "hi", "sys", "gpt-4o")
	if err != nil {
		t.Fatalf("openai complete: %v", err)
	}
	if out != "openai reply" {
		t.Fatalf("want 'openai reply', got %q", out)
	}
	if srv.lastPath != "/v1/proxy/openai/v1/chat/completions" {
		t.Fatalf("wrong proxy path: %s", srv.lastPath)
	}
}

func TestComplete_SendsAnthropicMessagesBody(t *testing.T) {
	srv := newFakeLens()
	defer srv.Close()
	c := New(srv.URL, "k1")

	_, _ = c.Complete(context.Background(), "ws-1", "Write something", "Be concise", "claude-haiku-4-6")

	var body map[string]any
	if err := json.Unmarshal(srv.lastBody, &body); err != nil {
		t.Fatalf("body not json: %v\n%s", err, srv.lastBody)
	}
	if body["model"] != "claude-haiku-4-6" {
		t.Fatalf("wrong model: %v", body["model"])
	}
	if body["system"] != "Be concise" {
		t.Fatalf("wrong system: %v", body["system"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
}

func TestComplete_ReturnsErrorWhenLensDown(t *testing.T) {
	// Dead port: connection refused. The error must propagate so the
	// engine can degrade gracefully at the engine layer.
	c := New("http://127.0.0.1:1", "k1")
	c.httpClient.Timeout = 200 * time.Millisecond

	_, err := c.Complete(context.Background(), "ws-1", "x", "y", "claude-haiku-4-6")
	if err == nil {
		t.Fatal("expected error from dead Lens, got nil")
	}
}

func TestComplete_ReturnsErrorOn5xx(t *testing.T) {
	srv := newFakeLens()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	defer srv.Close()
	c := New(srv.URL, "k1")

	_, err := c.Complete(context.Background(), "ws-1", "x", "y", "claude-haiku-4-6")
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

func TestIsConfigured_FalseForEmptyURL(t *testing.T) {
	if New("", "k1").IsConfigured() {
		t.Fatal("expected unconfigured when URL empty")
	}
	if New("http://lens", "").IsConfigured() {
		t.Fatal("expected unconfigured when key empty")
	}
	if !New("http://lens", "k1").IsConfigured() {
		t.Fatal("expected configured")
	}
}

func TestComplete_FeatureHeaderOverridable(t *testing.T) {
	srv := newFakeLens()
	defer srv.Close()
	c := New(srv.URL, "k1")

	_, _ = c.CompleteWithFeature(context.Background(), "ws-1", "Hello", "sys", "claude-haiku-4-6", "docs-ai-summarize")
	if got := srv.lastHeaders.Get("X-Talyvor-Feature"); got != "docs-ai-summarize" {
		t.Fatalf("want docs-ai-summarize feature, got %q", got)
	}
}

func TestComplete_HandlesEmptyContent(t *testing.T) {
	srv := newFakeLens()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[]}`))
	}
	defer srv.Close()
	c := New(srv.URL, "k1")

	out, err := c.Complete(context.Background(), "ws-1", "x", "y", "claude-haiku-4-6")
	if err != nil {
		t.Fatalf("empty content: %v", err)
	}
	if !strings.HasPrefix(out, "") || out != "" {
		t.Fatalf("want empty string, got %q", out)
	}
}
