package collab

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

// An inbound frame bigger than the read limit closes the socket with 1009 instead of being buffered.
// The frame under the limit on the same socket is acked first, so the close is the limit and not a
// socket that never worked.
func TestCollab_OversizedFrame_ClosesTheSocket(t *testing.T) {
	engine := NewOTEngine()
	h := NewHandler(engine).
		WithAccess(stubResolver{inScope: true, canEdit: true, actor: "m-1"}).
		WithReadLimit(1024)
	r := chi.NewRouter()
	r.Get("/v1/collab/{pageID}/ws", h.ServeWS)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/collab/pg-1/ws?client_id=c-1"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	readUntil(t, conn, "init")

	sendChange(t, conn, "small", `{"type":"doc"}`, 0)
	if m := readUntil(t, conn, "ack"); m["type"] != "ack" {
		t.Fatalf("a frame under the limit was not acked: %v", m)
	}

	sendChange(t, conn, "big", `{"type":"doc","pad":"`+strings.Repeat("x", 2048)+`"}`, 1)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		if !errors.As(err, &ce) || ce.Code != websocket.CloseMessageTooBig {
			t.Fatalf("an oversized frame ended the read with %v, want close 1009 (message too big)", err)
		}
		break
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(engine.GetPresence("pg-1")) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the server still holds the session after closing it for an oversized frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
