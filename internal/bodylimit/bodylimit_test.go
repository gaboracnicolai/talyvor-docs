package bodylimit_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/bodylimit"
)

// No route in Docs caps its request body: `json.NewDecoder(r.Body).Decode(&in)` with no
// http.MaxBytesReader anywhere in the repo. PATCH /pages/{id} decodes into a
// map[string]any, so a multi-GB `content` is read fully into RAM, written to Postgres,
// walked by extractContentText, and fanned to the semantic indexer. Nothing rejects it, and
// docker-compose sets no memory limit.

func newChain(max int64, got *int) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		*got = len(b)
		if err != nil {
			// A capped read fails here for a chunked/lying client. The handler's own
			// decode-error path takes over; what matters is that the read STOPPED.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return bodylimit.Middleware(max, nil)(h)
}

func TestMiddleware_UnderCapPassesThrough(t *testing.T) {
	got := 0
	chain := newChain(1024, &got)

	body := strings.Repeat("a", 500)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("under-cap body = %d, want 200 (the cap must not break normal traffic)", rr.Code)
	}
	if got != 500 {
		t.Errorf("handler read %d bytes, want 500 — an under-cap body must arrive intact", got)
	}
}

// The declared-oversize case: any ordinary client (curl, fetch, the SPA) sends
// Content-Length, so this is the path that matters in practice.
func TestMiddleware_OverCapRejectedWith413(t *testing.T) {
	got := 0
	chain := newChain(1024, &got)

	body := strings.Repeat("a", 4096)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over-cap body = %d, want 413", rr.Code)
	}
	if got != 0 {
		t.Errorf("handler read %d bytes of an over-cap body, want 0 — it must be rejected before the "+
			"handler touches it, or the memory is already spent", got)
	}
}

// A client that lies about (or omits) Content-Length must still not be able to stream
// unbounded bytes into memory. The status is less precise here — the read fails and the
// handler's own error path answers — but the MEMORY BOUND, which is the security property,
// must hold.
func TestMiddleware_ChunkedOverCapIsStillBounded(t *testing.T) {
	got := 0
	chain := newChain(1024, &got)

	body := strings.Repeat("a", 64*1024)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.ContentLength = -1 // unknown length, as with chunked transfer-encoding
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if got > 1024 {
		t.Errorf("handler read %d bytes past a 1024-byte cap on an unknown-length body — a client that "+
			"omits Content-Length can exhaust memory, which defeats the whole cap", got)
	}
}

// A liar sending a small Content-Length but a large body must also be bounded.
func TestMiddleware_UnderstatedContentLengthIsStillBounded(t *testing.T) {
	got := 0
	chain := newChain(1024, &got)

	body := strings.Repeat("a", 64*1024)
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.ContentLength = 10 // lies
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if got > 1024 {
		t.Errorf("handler read %d bytes past the cap from a client understating Content-Length (%d "+
			"declared) — the cap must not trust the header", got, 10)
	}
}

// A GET with no body must not be disturbed.
func TestMiddleware_NoBodyUnaffected(t *testing.T) {
	got := 0
	chain := newChain(1024, &got)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("bodyless GET = %d, want 200", rr.Code)
	}
}
