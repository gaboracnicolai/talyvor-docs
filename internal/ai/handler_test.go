package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/lensintegration"
	"github.com/talyvor/docs/internal/model"
)

// fakePages stubs the page-store dependency the handler needs for
// the /ask endpoint. The handler should call Search to gather context
// pages then hand them off to the engine.
type fakePages struct {
	searched []string
	results  []model.Page
}

func (f *fakePages) Search(_ context.Context, _ /*workspaceID*/, q string, _ int) ([]model.Page, error) {
	f.searched = append(f.searched, q)
	return f.results, nil
}

// allowAllPages is an EXPLICIT allow-all read gate for the bare-handler tests in this package.
//
// /ask now REFUSES rather than answering when no gate is wired (Handler.WithPageRead), so these
// tests have to state which pages the caller may read instead of inheriting an unfiltered corpus
// by omission. Allow-all is the right answer for them — none of them is about access, they drive
// a `fakePages` with no database behind it, and the access decision is now visible in the fixture
// rather than implied. What the gate actually DECIDES is measured against real Postgres and the
// real permission engine in privatespace_realpg_test.go.
type allowAllPages struct{}

func (allowAllPages) AuthorizePageRead(context.Context, string) (bool, bool) { return true, true }

func newRouter(e *Engine, pages PageSearcher) http.Handler {
	r := chi.NewRouter()
	// Mirror production: the authz middleware stamps verified memberships before handlers run. Tests
	// call the AI endpoints as a member of ws-1 (the workspace in the URL).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authz.WithMemberships(req.Context(), "u@ws1.com", []authz.Membership{{WorkspaceID: "ws-1", MemberID: "m"}})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	r.Route("/v1", func(r chi.Router) {
		NewHandler(e, pages).WithPageRead(allowAllPages{}).Mount(r)
	})
	return r
}

func TestWriteEndpoint_ReturnsGeneratedText(t *testing.T) {
	srv := newAIFake(t, `{"content":[{"type":"text","text":"Generated body."}]}`)
	defer srv.Close()
	h := newRouter(New(meteredLensClient(srv.URL)), &fakePages{})

	body, _ := json.Marshal(map[string]string{"prompt": "explain caching", "context": "ctx"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/write", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["text"] != "Generated body." {
		t.Fatalf("unexpected response: %q", out["text"])
	}
}

func TestTransformEndpoint_RoutesByAction(t *testing.T) {
	srv := newAIFake(t, `{"content":[{"type":"text","text":"shorter"}]}`)
	defer srv.Close()
	h := newRouter(New(meteredLensClient(srv.URL)), &fakePages{})

	for _, action := range []string{"summarize", "grammar", "shorter", "longer"} {
		body, _ := json.Marshal(map[string]string{"action": action, "text": "input"})
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/transform", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("action %s: status %d body=%s", action, rr.Code, rr.Body.String())
		}
	}
}

func TestTransformEndpoint_RejectsUnknownAction(t *testing.T) {
	srv := newAIFake(t, `{"content":[]}`)
	defer srv.Close()
	h := newRouter(New(meteredLensClient(srv.URL)), &fakePages{})

	body, _ := json.Marshal(map[string]string{"action": "delete-everything", "text": "x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/transform", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}

func TestAskEndpoint_GathersPageContext(t *testing.T) {
	srv := newAIFake(t, `{"content":[{"type":"text","text":"Run make deploy."}]}`)
	defer srv.Close()
	pages := &fakePages{
		results: []model.Page{
			{ID: "p-1", Title: "Deploy guide", ContentText: "Run make deploy.", Slug: "deploy", SpaceID: "s-1"},
		},
	}
	h := newRouter(New(meteredLensClient(srv.URL)), pages)

	body, _ := json.Marshal(map[string]string{"question": "How do I deploy?"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/ask", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Answer  string `json:"answer"`
		Sources []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Answer != "Run make deploy." {
		t.Fatalf("unexpected answer: %q", out.Answer)
	}
	if len(out.Sources) != 1 {
		t.Fatalf("want 1 source, got %d", len(out.Sources))
	}
	if out.Sources[0].Title != "Deploy guide" {
		t.Fatalf("wrong source title: %q", out.Sources[0].Title)
	}
	if len(pages.searched) == 0 {
		t.Fatal("expected the search to be invoked for ask")
	}
}

// AN UNWIRED READ GATE MUST REFUSE, NOT ANSWER — the behaviour WithPageRead's header argues for,
// asserted rather than described.
//
// This is the one thing privatespace_realpg_test.go cannot see: it always wires a gate, because
// what it measures is what the gate DECIDES. The two alternatives to refusing are both silent —
// answering from every page in the workspace (the leak that guard exists for) or answering from
// none, which produces a confident reply from zero grounding that reads exactly like "your
// documentation does not cover this". /ask's four siblings take their text from the caller and
// read no corpus, so they are unaffected and that is asserted here too: a fix that gated the whole
// handler would break four working routes.
func TestAskEndpoint_RefusesWhenNoPageReadGateIsWired(t *testing.T) {
	srv := newAIFake(t, `{"content":[{"type":"text","text":"Run make deploy."}]}`)
	defer srv.Close()
	pages := &fakePages{
		results: []model.Page{
			{ID: "p-1", Title: "Deploy guide", ContentText: "Run make deploy.", Slug: "deploy", SpaceID: "s-1"},
		},
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := authz.WithMemberships(req.Context(), "u@ws1.com",
				[]authz.Membership{{WorkspaceID: "ws-1", MemberID: "m"}})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	// Deliberately NO WithPageRead — this is the production misconfiguration under test.
	r.Route("/v1", func(r chi.Router) { NewHandler(New(meteredLensClient(srv.URL)), pages).Mount(r) })

	do := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)
		return rr
	}

	rr := do("/v1/workspaces/ws-1/ai/ask", `{"question":"How do I deploy?"}`)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("[B-NILGATE] /ask with no read gate = %d, want 500. It answered instead of "+
			"refusing: %s", rr.Code, rr.Body.String())
	}
	if len(pages.searched) != 0 {
		t.Errorf("[B-NILSTORE] /ask with no read gate still ran the corpus search (%v) — the "+
			"refusal must come BEFORE the store op, or an unfiltered result set exists in "+
			"process whether or not it is returned", pages.searched)
	}

	// THE OTHER FOUR ROUTES READ NO CORPUS AND MUST BE UNTOUCHED.
	for _, c := range []struct{ path, body string }{
		{"/v1/workspaces/ws-1/ai/write", `{"prompt":"draft"}`},
		{"/v1/workspaces/ws-1/ai/transform", `{"action":"summarize","text":"hello"}`},
		{"/v1/workspaces/ws-1/ai/translate", `{"text":"hello","target_language":"French"}`},
		{"/v1/workspaces/ws-1/ai/suggest-title", `{"content":"body"}`},
	} {
		if rr := do(c.path, c.body); rr.Code != http.StatusOK {
			t.Errorf("[B-SIBLING] %s with no read gate = %d, want 200 — it takes its text from "+
				"the caller and reads no corpus, so the gate is none of its business: %s",
				c.path, rr.Code, rr.Body.String())
		}
	}
}

func TestWriteEndpoint_503WhenLensUnconfigured(t *testing.T) {
	h := newRouter(New(lensintegration.New("", "")), &fakePages{})

	body, _ := json.Marshal(map[string]string{"prompt": "x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/write", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
	// Body must not leak raw lens error strings — only a friendly
	// "AI unavailable" message.
	if strings.Contains(rr.Body.String(), "lens:") {
		t.Fatalf("raw error leaked: %s", rr.Body.String())
	}
}

func TestSuggestTitleEndpoint(t *testing.T) {
	srv := newAIFake(t, `{"content":[{"type":"text","text":"Deploy Pipeline Overview"}]}`)
	defer srv.Close()
	h := newRouter(New(meteredLensClient(srv.URL)), &fakePages{})

	body, _ := json.Marshal(map[string]string{"content": "long content"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/suggest-title", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["title"] != "Deploy Pipeline Overview" {
		t.Fatalf("unexpected: %q", out["title"])
	}
}

// newAIFake is a one-shot httptest server returning the canned
// Anthropic-shaped JSON. Handler tests use this so the engine is
// exercised end-to-end (real HTTP, real JSON parsing) without a live
// Lens.
func newAIFake(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Completions mint a per-workspace token first; serve the mint endpoint.
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		_, _ = w.Write([]byte(response))
	}))
}
