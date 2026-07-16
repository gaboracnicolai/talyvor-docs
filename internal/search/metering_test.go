package search

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
)

// ─── a faithful fake Lens for the credential-seam proofs ────────────────────
//
// It serves BOTH endpoints Docs now hits:
//   - POST /v1/auth/token  → mints a JWT whose workspace_id claim = the requested workspace,
//     recording the workspace and the admin key it was minted with.
//   - the embeddings data path → decodes the presented bearer's workspace_id claim and
//     records it, so a test can prove which workspace a data-path call was attributed to.
// The JWT is hand-rolled (HS256) so the fake can both mint and decode it without a JWT lib —
// Docs itself never parses the token; it is opaque on the Docs side.

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func makeJWT(ws string) string {
	header := b64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := b64url([]byte(fmt.Sprintf(`{"workspace_id":%q}`, ws)))
	mac := hmac.New(sha256.New, []byte("fake-lens-secret"))
	mac.Write([]byte(header + "." + payload))
	sig := b64url(mac.Sum(nil))
	return header + "." + payload + "." + sig
}

// claimWS decodes the workspace_id claim out of a hand-rolled JWT (payload segment only).
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

type jwtFakeLens struct {
	*httptest.Server

	mu         sync.Mutex
	mintWS     []string // workspace_id seen at the mint endpoint
	mintAuth   []string // Authorization seen at the mint endpoint (must be the admin key)
	dataAuth   []string // raw Authorization seen on data-path calls
	dataClaims []string // decoded workspace_id from each data-path bearer
	mintFail   bool     // when true, the mint endpoint 500s (fail-mode tests)
}

func newJWTFakeLens(t *testing.T) *jwtFakeLens {
	t.Helper()
	f := &jwtFakeLens{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/token" {
			var body struct {
				WorkspaceID string `json:"workspace_id"`
				TTLHours    int    `json:"ttl_hours"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.mintWS = append(f.mintWS, body.WorkspaceID)
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
		// Data path (embeddings): record who the bearer says it is, then return a vector.
		auth := r.Header.Get("Authorization")
		f.mu.Lock()
		f.dataAuth = append(f.dataAuth, auth)
		f.dataClaims = append(f.dataClaims, claimWS(strings.TrimPrefix(auth, "Bearer ")))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	return f
}

func (f *jwtFakeLens) snapshot() (mintWS, mintAuth, dataAuth, dataClaims []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mintWS...),
		append([]string(nil), f.mintAuth...),
		append([]string(nil), f.dataAuth...),
		append([]string(nil), f.dataClaims...)
}

// meteredSemantic builds a SemanticSearch wired to the per-workspace provider (admin key
// "GLOBAL-ADMIN-KEY") pointed at the fake Lens, plus a pgxmock pool.
func meteredSemantic(t *testing.T, lensURL string) (*SemanticSearch, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	client := lensintegration.New(lensURL, "GLOBAL-ADMIN-KEY")
	prov := lenscreds.New(lensURL, "GLOBAL-ADMIN-KEY", lenscreds.Options{})
	s := newSemanticSearch(client, pool).WithLensURL(lensURL).WithTokenProvider(prov)
	return s, pool
}

// PROPERTY 2 — the index/embed data-path request carries a per-workspace JWT (claim = the
// save's workspace), and NEVER the raw global admin key.
func TestIndexEmbed_CarriesPerWorkspaceJWT_NotGlobalKey(t *testing.T) {
	f := newJWTFakeLens(t)
	defer f.Close()
	s, pool := meteredSemantic(t, f.URL)

	pool.ExpectExec(`INSERT INTO page_embeddings`).
		WithArgs("pg-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := s.IndexPage(context.Background(), "pg-1", "wsA", "auth flow body"); err != nil {
		t.Fatalf("IndexPage: %v", err)
	}

	_, mintAuth, dataAuth, dataClaims := f.snapshot()
	if len(dataAuth) != 1 {
		t.Fatalf("want 1 data-path call, got %d", len(dataAuth))
	}
	// The embed bearer must be the minted per-workspace JWT, decoding to wsA...
	if dataClaims[0] != "wsA" {
		t.Fatalf("embed bearer decoded to workspace %q, want wsA", dataClaims[0])
	}
	// ...and must NOT be the raw global admin key.
	if dataAuth[0] == "Bearer GLOBAL-ADMIN-KEY" {
		t.Fatal("embed carried the raw global admin key — per-workspace metering defeated")
	}
	// The admin key is the MINTING credential only.
	if len(mintAuth) == 0 || mintAuth[0] != "Bearer GLOBAL-ADMIN-KEY" {
		t.Fatalf("mint endpoint saw Authorization %v, want the admin key to mint", mintAuth)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// PROPERTY 2 (search side) — the semantic SEARCH embed request carries the per-workspace JWT
// for the searching workspace, never the global key.
func TestSearchEmbed_CarriesPerWorkspaceJWT_NotGlobalKey(t *testing.T) {
	f := newJWTFakeLens(t)
	defer f.Close()
	s, pool := meteredSemantic(t, f.URL)

	rows := pgxmock.NewRows([]string{"page_id", "similarity"}).AddRow("pg-1", float64(0.93))
	pool.ExpectQuery(`page_embeddings.*<=>`).
		WithArgs(pgxmock.AnyArg(), "wsB", 10).
		WillReturnRows(rows)

	if _, err := s.Search(context.Background(), "wsB", "auth", 10); err != nil {
		t.Fatalf("Search: %v", err)
	}

	_, _, dataAuth, dataClaims := f.snapshot()
	if len(dataClaims) != 1 || dataClaims[0] != "wsB" {
		t.Fatalf("search embed bearer decoded to %v, want [wsB]", dataClaims)
	}
	if dataAuth[0] == "Bearer GLOBAL-ADMIN-KEY" {
		t.Fatal("search embed carried the raw global admin key")
	}
}

// PROPERTY 2 fail mode (SEARCH = FAIL-CLOSED): when a per-workspace token cannot be minted,
// Search returns ErrTokenUnavailable (NOT empty results), and NO data-path request is sent —
// so the global key is never used as a fallback bearer.
func TestSearch_FailsClosedOnMintFailure(t *testing.T) {
	f := newJWTFakeLens(t)
	f.mintFail = true
	defer f.Close()
	s, _ := meteredSemantic(t, f.URL) // no pgvector query expected — embed bails before the DB

	out, err := s.Search(context.Background(), "wsA", "auth", 10)
	if !errors.Is(err, ErrTokenUnavailable) {
		t.Fatalf("Search returned err=%v out=%v, want ErrTokenUnavailable (fail-closed)", err, out)
	}
	_, _, dataAuth, _ := f.snapshot()
	if len(dataAuth) != 0 {
		t.Fatalf("a data-path request went out despite the mint failure: %v — the global key must never ride the data path", dataAuth)
	}
}

// PROPERTY 2 fail mode (INDEX = BEST-EFFORT): when a per-workspace token cannot be minted,
// IndexPage returns nil (no error bubbles into the save path), writes nothing, and sends NO
// data-path request. The page re-indexes on its next save via the throttle's re-enqueue.
func TestIndexPage_BestEffortOnMintFailure(t *testing.T) {
	f := newJWTFakeLens(t)
	f.mintFail = true
	defer f.Close()
	s, pool := meteredSemantic(t, f.URL) // no Exec expected — embed bails before the DB

	if err := s.IndexPage(context.Background(), "pg-1", "wsA", "body"); err != nil {
		t.Fatalf("IndexPage must stay best-effort on a mint failure, got err=%v", err)
	}
	_, _, dataAuth, _ := f.snapshot()
	if len(dataAuth) != 0 {
		t.Fatalf("embed was attempted despite the mint failure: %v — never fall back to the global key", dataAuth)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// PROPERTY 3 — THE DECISIVE PROOF. Two tenants driven end-to-end through one SemanticSearch
// (one shared provider) against a fake Lens that mints per-workspace JWTs and decodes the
// bearer's workspace_id claim on every data-path call. Sequence: wsA index, wsA search, wsB
// index, wsB search. It proves, together with the merged Lens-side proof (per-ws JWT →
// isolated rate-limit bucket + isolated COGS), the whole chain:
//   - each data-path call carries a JWT claiming the workspace that made it (wsA↔wsA, wsB↔wsB);
//   - the mint endpoint was called with the ADMIN key, once per workspace (cached across the
//     tenant's index + search);
//   - NO data-path call ever carries the raw global admin key.
func TestTwoTenants_DecisiveEndToEndIsolation(t *testing.T) {
	f := newJWTFakeLens(t)
	defer f.Close()
	s, pool := meteredSemantic(t, f.URL)

	// pgxmock enforces expectation order — declare them in the real call sequence: wsA index
	// (upsert), wsA search (pgvector query scoped to wsA), then the same pair for wsB.
	pool.ExpectExec(`INSERT INTO page_embeddings`).WithArgs("pgA", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectQuery(`page_embeddings.*<=>`).WithArgs(pgxmock.AnyArg(), "wsA", 10).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "similarity"}).AddRow("pgA", float64(0.9)))
	pool.ExpectExec(`INSERT INTO page_embeddings`).WithArgs("pgB", pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectQuery(`page_embeddings.*<=>`).WithArgs(pgxmock.AnyArg(), "wsB", 10).
		WillReturnRows(pgxmock.NewRows([]string{"page_id", "similarity"}).AddRow("pgB", float64(0.9)))

	ctx := context.Background()
	if err := s.IndexPage(ctx, "pgA", "wsA", "alpha body"); err != nil {
		t.Fatalf("wsA index: %v", err)
	}
	if _, err := s.Search(ctx, "wsA", "alpha", 10); err != nil {
		t.Fatalf("wsA search: %v", err)
	}
	if err := s.IndexPage(ctx, "pgB", "wsB", "beta body"); err != nil {
		t.Fatalf("wsB index: %v", err)
	}
	if _, err := s.Search(ctx, "wsB", "beta", 10); err != nil {
		t.Fatalf("wsB search: %v", err)
	}

	mintWS, mintAuth, dataAuth, dataClaims := f.snapshot()

	// Four data-path calls, each attributed (by decoded JWT claim) to the workspace that made
	// it — in order: wsA index, wsA search, wsB index, wsB search.
	want := []string{"wsA", "wsA", "wsB", "wsB"}
	if len(dataClaims) != len(want) {
		t.Fatalf("got %d data-path calls %v, want %d %v", len(dataClaims), dataClaims, len(want), want)
	}
	for i := range want {
		if dataClaims[i] != want[i] {
			t.Fatalf("data-path call %d attributed to %q, want %q — cross-tenant leak; full=%v", i, dataClaims[i], want[i], dataClaims)
		}
	}
	// The raw global admin key NEVER rode a data-path request.
	for _, a := range dataAuth {
		if a == "Bearer GLOBAL-ADMIN-KEY" {
			t.Fatalf("the global admin key appeared on a data-path call: %v", dataAuth)
		}
	}
	// The mint endpoint was called with the ADMIN key, once per workspace (the tenant's search
	// reused the token its index minted — the cache held end-to-end).
	for _, a := range mintAuth {
		if a != "Bearer GLOBAL-ADMIN-KEY" {
			t.Fatalf("mint was called with %q, want the admin key 'Bearer GLOBAL-ADMIN-KEY'", a)
		}
	}
	mintCount := map[string]int{}
	for _, w := range mintWS {
		mintCount[w]++
	}
	if mintCount["wsA"] != 1 || mintCount["wsB"] != 1 || len(mintCount) != 2 {
		t.Fatalf("mint counts = %v, want exactly one mint per workspace (cached across index+search)", mintCount)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
