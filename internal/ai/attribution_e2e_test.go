package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ⚠ THE END-TO-END RED: A DOCUMENT'S OWN AI COST MUST REACH THAT DOCUMENT.
//
// The storage tests in internal/page prove the LEDGER — bind a request, price it, the money lands.
// They passed the first time they ran, because the migration and the store were written together,
// so they never demonstrated the defect. What was actually broken was one layer up: every AI
// handler already received a `page_id` in its request body and DISCARDED it, so no binding was
// ever created and own_ai_cost_usd could only ever have been zero.
//
// These tests drive the real HTTP handler with a real request body and assert that the binding
// was recorded with the right page, workspace and operation. They are what fails if the threading
// is removed — see TestAttribution_PositiveControl_NeuteredThreadingIsCaught for the proof that
// they can still see that state, since the threading was written before the test.

// recordingBinder captures what the engine bound, so the assertion is on the RECORDED FACT rather
// than on a return value or a status code.
type recordingBinder struct {
	mu    sync.Mutex
	binds []bindCall
}

type bindCall struct{ requestID, pageID, workspaceID, operation string }

func (b *recordingBinder) BindAISpend(_ context.Context, requestID, pageID, workspaceID, operation string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.binds = append(b.binds, bindCall{requestID, pageID, workspaceID, operation})
	return nil
}

func (b *recordingBinder) all() []bindCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bindCall(nil), b.binds...)
}

// lensStub reuses the package's existing fakeLens (which already serves the per-workspace mint
// endpoint) and adds the one thing this feature turns on: X-Talyvor-Request-ID. Reusing it rather
// than standing up a third Lens double keeps the two suites agreeing about what Lens looks like.
func lensStub(t *testing.T, requestID string) *fakeLens {
	t.Helper()
	f := newFakeLens(t)
	t.Cleanup(f.Close)
	f.respond = func(w http.ResponseWriter, _ *http.Request) {
		if requestID != "" {
			w.Header().Set("X-Talyvor-Request-ID", requestID)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}
	return f
}

// post drives the mounted handler exactly as the editor does, through the SAME authz-stamping
// router the package's other handler tests use — so this suite cannot pass on an auth posture
// production does not have.
func post(t *testing.T, e *Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := newRouter(e, &fakePages{})
	req := httptest.NewRequest(http.MethodPost, "/v1"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// ⚠ THE HEADLINE. Every operation that belongs to ONE page must bind that page.
func TestAttribution_EachSinglePageOperationBindsItsPage(t *testing.T) {
	cases := []struct {
		name, path, body, wantOp string
	}{
		{"write", "/workspaces/ws-1/ai/write",
			`{"prompt":"draft","context":"","page_id":"pg-write"}`, "docs-ai-write"},
		{"transform/summarize", "/workspaces/ws-1/ai/transform",
			`{"action":"summarize","text":"hello","page_id":"pg-sum"}`, "docs-ai-summarize"},
		{"transform/grammar", "/workspaces/ws-1/ai/transform",
			`{"action":"grammar","text":"hello","page_id":"pg-gram"}`, "docs-ai-grammar"},
		{"translate", "/workspaces/ws-1/ai/translate",
			`{"text":"hello","language":"fr","page_id":"pg-tr"}`, "docs-ai-translate"},
		{"suggest-title", "/workspaces/ws-1/ai/suggest-title",
			`{"content":"body","page_id":"pg-title"}`, "docs-ai-title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := lensStub(t, "req-"+tc.name)
			b := &recordingBinder{}
			e := New(meteredLensClient(srv.URL)).WithSpendBinder(b)

			rec := post(t, e, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
			}

			got := b.all()
			if len(got) != 1 {
				t.Fatalf("recorded %d bindings, want 1. The handler received page_id in the request "+
					"body and did not attribute the call to it — so this document's own AI cost lands "+
					"nowhere, which is the entire finding.", len(got))
			}
			var wantPage string
			_ = json.Unmarshal([]byte(tc.body), &struct {
				PageID *string `json:"page_id"`
			}{&wantPage})
			if got[0].pageID != wantPage {
				t.Errorf("bound page_id = %q, want %q", got[0].pageID, wantPage)
			}
			if got[0].workspaceID != "ws-1" {
				t.Errorf("bound workspace_id = %q, want ws-1 — attribution must be tenant-scoped", got[0].workspaceID)
			}
			if got[0].operation != tc.wantOp {
				t.Errorf("bound operation = %q, want %q", got[0].operation, tc.wantOp)
			}
			if got[0].requestID != "req-"+tc.name {
				t.Errorf("bound request_id = %q, want Lens's X-Talyvor-Request-ID", got[0].requestID)
			}
		})
	}
}

// ⚠ THE HONEST SCOPE, ASSERTED SO IT CANNOT DRIFT. `ask` draws on several pages, so attributing it
// to one would be a fabrication. It must bind NOTHING — and a future edit that "helpfully" starts
// attributing it to the first source page reds here.
func TestAttribution_AskSpansPagesAndBindsNothing(t *testing.T) {
	srv := lensStub(t, "req-ask")
	b := &recordingBinder{}
	e := New(meteredLensClient(srv.URL)).WithSpendBinder(b)

	rec := post(t, e, "/workspaces/ws-1/ai/ask", `{"question":"how does auth work"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := b.all(); len(got) != 0 {
		t.Fatalf("ask recorded %d bindings (%+v), want 0. An answer drawn from several pages belongs "+
			"to none of them; attributing it to one would invent a fact.", len(got), got)
	}
}

// A page-less call — a title suggested before the page exists — must not invent a binding.
func TestAttribution_NoPageIDBindsNothing(t *testing.T) {
	srv := lensStub(t, "req-nopage")
	b := &recordingBinder{}
	e := New(meteredLensClient(srv.URL)).WithSpendBinder(b)

	if rec := post(t, e, "/workspaces/ws-1/ai/suggest-title", `{"content":"body"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := b.all(); len(got) != 0 {
		t.Fatalf("a call with no page_id recorded %d bindings, want 0", len(got))
	}
}

// An older Lens that sends no request id must not produce a binding keyed on "" — that row would
// collide with every other unkeyed call on the primary key and mis-attribute their costs.
func TestAttribution_NoRequestIDHeaderBindsNothing(t *testing.T) {
	srv := lensStub(t, "") // no X-Talyvor-Request-ID
	b := &recordingBinder{}
	e := New(meteredLensClient(srv.URL)).WithSpendBinder(b)

	if rec := post(t, e, "/workspaces/ws-1/ai/write", `{"prompt":"d","page_id":"pg-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := b.all(); len(got) != 0 {
		t.Fatalf("recorded %d bindings with no request id from Lens, want 0 — an empty key would "+
			"collide across calls", len(got))
	}
}

// ⚠ A BOOKKEEPING FAILURE MUST NOT COST THE USER THEIR ANSWER. They asked for AI text and Lens
// already charged for it; failing the response because a ledger write failed would take the thing
// they paid for and give them an error.
func TestAttribution_BindFailureDoesNotFailTheRequest(t *testing.T) {
	srv := lensStub(t, "req-bindfail")
	e := New(meteredLensClient(srv.URL)).WithSpendBinder(failingBinder{})

	rec := post(t, e, "/workspaces/ws-1/ai/write", `{"prompt":"d","page_id":"pg-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a failed binding failed the whole request (status %d) — the completion was already "+
			"paid for", rec.Code)
	}
}

type failingBinder struct{}

func (failingBinder) BindAISpend(context.Context, string, string, string, string) error {
	return errors.New("ledger unavailable")
}
