package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
)

// fakeLens captures the last request and lets each test programme a
// response. We instantiate one fake per test and point a real
// lensintegration.Client at it so the engine round-trips through real
// HTTP code paths (header construction, JSON body, response parsing).
type fakeLens struct {
	*httptest.Server
	lastFeature string
	lastBody    map[string]any
	respond     func(w http.ResponseWriter, r *http.Request)
}

func newFakeLens(t *testing.T) *fakeLens {
	t.Helper()
	f := &fakeLens{}
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"OK"}]}`))
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Completions mint a per-workspace token first; serve the mint endpoint. The ai tests
		// don't assert on the token, so an opaque one suffices.
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		f.lastFeature = r.Header.Get("X-Talyvor-Feature")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &f.lastBody)
		f.respond(w, r)
	}))
	return f
}

// meteredLensClient wires the per-workspace provider (shared by the ai handler tests too) so
// completions carry a per-workspace bearer. The fake serves the mint endpoint.
func meteredLensClient(lensURL string) *lensintegration.Client {
	return lensintegration.New(lensURL, "k1").WithTokenProvider(lenscreds.New(lensURL, "k1", lenscreds.Options{}))
}

func newEngine(srv *fakeLens) *Engine {
	return New(meteredLensClient(srv.URL))
}

func TestWriteWithAI_ReturnsGeneratedText(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Here is a paragraph about caching."}]}`))
	}
	e := newEngine(srv)

	out, err := e.WriteWithAI(context.Background(), "ws-1", "explain caching", "surrounding text")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if out != "Here is a paragraph about caching." {
		t.Fatalf("unexpected output: %q", out)
	}
	if srv.lastFeature != "docs-ai-write" {
		t.Fatalf("expected feature docs-ai-write, got %q", srv.lastFeature)
	}
	// Body should pin the haiku model + ship the user-supplied context.
	if srv.lastBody["model"] != "claude-haiku-4-6" {
		t.Fatalf("want haiku, got %v", srv.lastBody["model"])
	}
	msgs, _ := srv.lastBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if !strings.Contains(content, "surrounding text") || !strings.Contains(content, "explain caching") {
		t.Fatalf("user prompt missing context: %q", content)
	}
}

func TestSummarize_ReturnsBullets(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"- one\n- two\n- three"}]}`))
	}
	e := newEngine(srv)

	out, err := e.Summarize(context.Background(), "ws-1", "long content goes here")
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if !strings.Contains(out, "- one") {
		t.Fatalf("expected bullets, got %q", out)
	}
	if srv.lastFeature != "docs-ai-summarize" {
		t.Fatalf("wrong feature: %q", srv.lastFeature)
	}
}

func TestFixGrammar_ReturnsCorrectedText(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"The cat sat on the mat."}]}`))
	}
	e := newEngine(srv)

	out, err := e.FixGrammar(context.Background(), "ws-1", "the cat sit on teh mat")
	if err != nil {
		t.Fatalf("grammar: %v", err)
	}
	if out != "The cat sat on the mat." {
		t.Fatalf("unexpected: %q", out)
	}
	if srv.lastFeature != "docs-ai-grammar" {
		t.Fatalf("wrong feature: %q", srv.lastFeature)
	}
}

func TestMakeShorter_AndLonger_UseHaikuAndDifferentFeatures(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	e := newEngine(srv)

	if _, err := e.MakeShorter(context.Background(), "ws-1", "x"); err != nil {
		t.Fatalf("shorter: %v", err)
	}
	if srv.lastFeature != "docs-ai-shorter" {
		t.Fatalf("wrong feature shorter: %q", srv.lastFeature)
	}

	if _, err := e.MakeLonger(context.Background(), "ws-1", "x"); err != nil {
		t.Fatalf("longer: %v", err)
	}
	if srv.lastFeature != "docs-ai-longer" {
		t.Fatalf("wrong feature longer: %q", srv.lastFeature)
	}
}

func TestTranslate_SendsTargetLanguageInSystemPrompt(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	e := newEngine(srv)

	if _, err := e.Translate(context.Background(), "ws-1", "hello", "Spanish"); err != nil {
		t.Fatalf("translate: %v", err)
	}
	system, _ := srv.lastBody["system"].(string)
	if !strings.Contains(system, "Spanish") {
		t.Fatalf("language not in system prompt: %q", system)
	}
	if srv.lastFeature != "docs-ai-translate" {
		t.Fatalf("wrong feature: %q", srv.lastFeature)
	}
}

func TestAskDocs_UsesSonnetAndPageContext(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"From the deploy guide: run make deploy."}]}`))
	}
	e := newEngine(srv)

	pages := []PageContext{
		{Title: "Deploy guide", Content: "Run make deploy to ship.", URL: "https://docs/d"},
		{Title: "On-call", Content: "Pageable services list.", URL: "https://docs/o"},
	}
	answer, err := e.AskDocs(context.Background(), "ws-1", "How do I deploy?", pages)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !strings.Contains(answer, "deploy") {
		t.Fatalf("answer missing deploy: %q", answer)
	}
	if srv.lastBody["model"] != "claude-sonnet-4-6" {
		t.Fatalf("AskDocs must use sonnet, got %v", srv.lastBody["model"])
	}
	if srv.lastFeature != "docs-ai-ask" {
		t.Fatalf("wrong feature: %q", srv.lastFeature)
	}
	msgs, _ := srv.lastBody["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].(string)
	if !strings.Contains(content, "Deploy guide") || !strings.Contains(content, "make deploy") {
		t.Fatalf("page context not included: %q", content)
	}
}

func TestSuggestTitle_TrimsQuotesAndWhitespace(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"  \"Deploy Pipeline Overview\"  "}]}`))
	}
	e := newEngine(srv)

	title, err := e.SuggestTitle(context.Background(), "ws-1", "content")
	if err != nil {
		t.Fatalf("title: %v", err)
	}
	if title != "Deploy Pipeline Overview" {
		t.Fatalf("expected trimmed title, got %q", title)
	}
}

func TestIsAvailable_FalseWhenLensUnconfigured(t *testing.T) {
	e := New(lensintegration.New("", ""))
	if e.IsAvailable() {
		t.Fatal("expected unavailable when Lens unconfigured")
	}
}

func TestEngine_DegradesGracefullyWhenLensDown(t *testing.T) {
	// Point the engine at a closed server so every call should hit a
	// connection error. The engine wraps that into a friendly
	// ErrLensUnavailable so handler code can map it to a 503.
	srv := newFakeLens(t)
	srv.Close()
	e := newEngine(srv)

	_, err := e.WriteWithAI(context.Background(), "ws-1", "x", "y")
	if err == nil {
		t.Fatal("expected error when Lens unreachable")
	}
}
