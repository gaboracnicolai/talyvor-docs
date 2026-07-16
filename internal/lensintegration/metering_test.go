package lensintegration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talyvor/docs/internal/lenscreds"
)

// Hand-rolled JWT so the fake Lens can both mint and decode the workspace_id claim without a
// JWT lib — Docs never parses the token; it is opaque on the Docs side.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func makeJWT(ws string) string {
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := b64url([]byte(fmt.Sprintf(`{"workspace_id":%q}`, ws)))
	mac := hmac.New(sha256.New, []byte("fake-lens-secret"))
	mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." + b64url(mac.Sum(nil))
}

func claimWS(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.WorkspaceID
}

// jwtFakeLens serves the mint endpoint and the proxy data path, recording the admin key used
// to mint and the decoded workspace_id claim presented on each data-path call.
type jwtFakeLens struct {
	*httptest.Server
	mu         sync.Mutex
	mintAuth   []string
	dataAuth   []string
	dataClaims []string
	mintFail   bool
}

func newJWTFakeLens(t *testing.T) *jwtFakeLens {
	t.Helper()
	f := &jwtFakeLens{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			var body struct {
				WorkspaceID string `json:"workspace_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.mintAuth = append(f.mintAuth, r.Header.Get("Authorization"))
			fail := f.mintFail
			f.mu.Unlock()
			if fail {
				http.Error(w, "mint unavailable", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      makeJWT(body.WorkspaceID),
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		auth := r.Header.Get("Authorization")
		f.mu.Lock()
		f.dataAuth = append(f.dataAuth, auth)
		f.dataClaims = append(f.dataClaims, claimWS(strings.TrimPrefix(auth, "Bearer ")))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	return f
}

func (f *jwtFakeLens) snapshot() (mintAuth, dataAuth, dataClaims []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mintAuth...),
		append([]string(nil), f.dataAuth...),
		append([]string(nil), f.dataClaims...)
}

func meteredClient(lensURL string) *Client {
	prov := lenscreds.New(lensURL, "GLOBAL-ADMIN-KEY", lenscreds.Options{})
	return New(lensURL, "GLOBAL-ADMIN-KEY").WithTokenProvider(prov)
}

// PROPERTY 4 — a completion request for workspace W carries a per-workspace JWT (claim = W),
// never the raw global admin key; the mint endpoint was called with the admin key.
func TestComplete_CarriesPerWorkspaceJWT_NotGlobalKey(t *testing.T) {
	f := newJWTFakeLens(t)
	defer f.Close()
	c := meteredClient(f.URL)

	if _, err := c.Complete(context.Background(), "wsA", "hi", "sys", "claude-haiku-4-6"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	mintAuth, dataAuth, dataClaims := f.snapshot()
	if len(dataClaims) != 1 || dataClaims[0] != "wsA" {
		t.Fatalf("completion bearer decoded to %v, want [wsA]", dataClaims)
	}
	if dataAuth[0] == "Bearer GLOBAL-ADMIN-KEY" {
		t.Fatal("completion carried the raw global admin key — per-workspace metering defeated")
	}
	if len(mintAuth) == 0 || mintAuth[0] != "Bearer GLOBAL-ADMIN-KEY" {
		t.Fatalf("mint endpoint saw %v, want the admin key to mint", mintAuth)
	}
}

// PROPERTY 4 (fail-closed) — a mint failure makes Complete ERROR (never fall back to the
// global key), and NO data-path request is sent.
func TestComplete_FailsClosedOnMintFailure(t *testing.T) {
	f := newJWTFakeLens(t)
	f.mintFail = true
	defer f.Close()
	c := meteredClient(f.URL)

	if _, err := c.Complete(context.Background(), "wsA", "hi", "sys", "claude-haiku-4-6"); err == nil {
		t.Fatal("Complete must error when the per-workspace token can't be minted (fail-closed)")
	}
	if _, dataAuth, _ := f.snapshot(); len(dataAuth) != 0 {
		t.Fatalf("a data-path request went out despite the mint failure: %v — never fall back to the global key", dataAuth)
	}
}
