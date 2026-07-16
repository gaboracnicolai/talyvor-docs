package search

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

	"github.com/pashagolub/pgxmock/v4"

	"github.com/talyvor/docs/internal/lenscreds"
	"github.com/talyvor/docs/internal/lensintegration"
)

// fakeLens mocks the Lens embeddings proxy. Each test wires its own
// response so we can simulate success, failure, and threshold-edge
// payloads.
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
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The embed path now mints a per-workspace token first. Serve the mint endpoint so
		// these tests exercise the real provider→embed flow; the data-path assertions below
		// are unchanged.
		if r.URL.Path == "/v1/auth/token" {
			var body struct {
				WorkspaceID string `json:"workspace_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"jwt-%s","expires_at":%q}`,
				body.WorkspaceID, time.Now().Add(time.Hour).Format(time.RFC3339))))
			return
		}
		f.lastFeature = r.Header.Get("X-Talyvor-Feature")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &f.lastBody)
		f.respond(w, r)
	}))
	return f
}

func newSemantic(t *testing.T, lensURL string) (*SemanticSearch, pgxmock.PgxPoolIface) {
	t.Helper()
	pool, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	client := lensintegration.New(lensURL, "k1")
	prov := lenscreds.New(lensURL, "k1", lenscreds.Options{})
	s := newSemanticSearch(client, pool).WithLensURL(lensURL).WithTokenProvider(prov)
	return s, pool
}

func TestIndexPage_CallsLensEmbeddingsAndUpserts(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)

	pool.ExpectExec(`INSERT INTO page_embeddings`).
		WithArgs("pg-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := s.IndexPage(context.Background(), "pg-1", "ws-1", "Auth flow doc body"); err != nil {
		t.Fatalf("IndexPage: %v", err)
	}
	if srv.lastFeature != "docs-search" {
		t.Fatalf("expected docs-search feature header, got %q", srv.lastFeature)
	}
	if input, _ := srv.lastBody["input"].(string); !strings.Contains(input, "Auth flow") {
		t.Fatalf("input not forwarded: %v", srv.lastBody)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestIndexPage_NoopWhenLensUnconfigured(t *testing.T) {
	s, pool := newSemantic(t, "") // empty URL → unconfigured
	// No SQL expected; pool.ExpectationsWereMet would fail if Upsert
	// fired regardless of the no-op contract.
	if err := s.IndexPage(context.Background(), "pg-1", "ws-1", "body"); err != nil {
		t.Fatalf("IndexPage with no Lens: %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestIndexPage_GracefulOnLensError(t *testing.T) {
	srv := newFakeLens(t)
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)
	// No DB write expected — the upstream call failed.
	if err := s.IndexPage(context.Background(), "pg-1", "ws-1", "x"); err != nil {
		t.Fatalf("expected graceful (nil), got %v", err)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSearch_ReturnsResultsAboveThreshold(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)

	rows := pgxmock.NewRows([]string{"page_id", "similarity"}).
		AddRow("pg-1", float64(0.93)).
		AddRow("pg-2", float64(0.81)).
		AddRow("pg-3", float64(0.50))
	pool.ExpectQuery(`page_embeddings.*<=>`).
		WithArgs(pgxmock.AnyArg(), "ws-1", 10).
		WillReturnRows(rows)

	out, err := s.Search(context.Background(), "ws-1", "auth", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// pg-3 is below 0.75 threshold so it must be dropped.
	if len(out) != 2 {
		t.Fatalf("want 2 above threshold, got %d (%+v)", len(out), out)
	}
	if out[0].PageID != "pg-1" {
		t.Fatalf("ordering wrong: %+v", out)
	}
}

func TestSearch_ReturnsEmptyWhenLensUnconfigured(t *testing.T) {
	s, pool := newSemantic(t, "")
	out, err := s.Search(context.Background(), "ws-1", "x", 10)
	if err != nil {
		t.Fatalf("Search no-lens: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want empty, got %+v", out)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSearch_GracefulWhenLensDown(t *testing.T) {
	srv := newFakeLens(t)
	srv.respond = func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)

	out, err := s.Search(context.Background(), "ws-1", "x", 10)
	if err != nil {
		t.Fatalf("Search must degrade gracefully: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("want empty on Lens failure, got %+v", out)
	}
	if err := pool.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestIndexPage_IdempotentOnUpsertConflict(t *testing.T) {
	srv := newFakeLens(t)
	defer srv.Close()
	s, pool := newSemantic(t, srv.URL)

	// ON CONFLICT updates the embedding + timestamp. The mock just
	// needs to confirm the INSERT runs without error twice.
	pool.ExpectExec(`INSERT INTO page_embeddings`).
		WithArgs("pg-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	pool.ExpectExec(`INSERT INTO page_embeddings`).
		WithArgs("pg-1", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	if err := s.IndexPage(context.Background(), "pg-1", "ws-1", "first"); err != nil {
		t.Fatalf("first index: %v", err)
	}
	if err := s.IndexPage(context.Background(), "pg-1", "ws-1", "second"); err != nil {
		t.Fatalf("second index: %v", err)
	}
}
