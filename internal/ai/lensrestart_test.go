package ai

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
)

// lensrestart_test.go — B17.88: translating a page while Lens restarts answers with the
// translation once Lens is back, and a call Lens did receive is never sent twice.

// countingLens serves the token mint and counts every completion that reaches it.
func countingLens(completions *atomic.Int32, answer func(w http.ResponseWriter)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			return
		}
		completions.Add(1)
		answer(w)
	})
}

// restartingLensClient is the production wiring (cmd/docs/main.go): the mint and the completion
// both go through the restart-tolerant transport.
func restartingLensClient(lensURL string) *lensintegration.Client {
	mint := &http.Client{Timeout: 10 * time.Second, Transport: lensintegration.RestartTolerant(nil)}
	return lensintegration.New(lensURL, "k1").
		WithTokenProvider(lenscreds.New(lensURL, "k1", lenscreds.Options{HTTP: mint}))
}

func translate(t *testing.T, lensURL string) *httptest.ResponseRecorder {
	t.Helper()
	h := newRouter(New(restartingLensClient(lensURL)), &fakePages{})
	body, _ := json.Marshal(map[string]string{"text": "The access code is QX-4215.", "language": "French"})
	req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws-1/ai/translate", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestTranslateEndpoint_AnswersOnceARestartingLensIsBack(t *testing.T) {
	// An address with nothing listening on it: every dial is refused, as while Lens is replaced.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var completions atomic.Int32
	lens := httptest.NewUnstartedServer(countingLens(&completions, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Le code d'accès est QX-4215."}]}`))
	}))
	t.Cleanup(lens.Close)
	go func() {
		time.Sleep(600 * time.Millisecond) // Lens comes back
		back, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("relisten on %s: %v", addr, err)
			return
		}
		lens.Listener = back
		lens.Start()
	}()

	rr := translate(t, "http://"+addr)
	if rr.Code != http.StatusOK {
		t.Fatalf("translate while Lens restarted: status %d %s — the reader is told nothing was asked of the model", rr.Code, rr.Body.String())
	}
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["text"] != "Le code d'accès est QX-4215." {
		t.Fatalf("translation = %q, want the French text with QX-4215 kept", out["text"])
	}
	if n := completions.Load(); n != 1 {
		t.Fatalf("Lens received %d completions, want exactly 1 — one translation, charged once", n)
	}
}

func TestTranslateEndpoint_ACallLensReceivedIsNotSentAgain(t *testing.T) {
	var completions atomic.Int32
	lens := httptest.NewServer(countingLens(&completions, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream LLM error"}`))
	}))
	t.Cleanup(lens.Close)

	rr := translate(t, lens.URL)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 for a call Lens refused: %s", rr.Code, rr.Body.String())
	}
	if n := completions.Load(); n != 1 {
		t.Fatalf("Lens received %d completions, want 1 — a call that reached Lens may already be priced, so it is never resent", n)
	}
}
