// Package search owns the unified search machinery: full-text via
// the page store, semantic via Lens embeddings + pgvector, and the
// HTTP handler that merges both. Semantic search is opt-in — if
// Lens is unconfigured, the package quietly degrades to full-text
// only.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talyvor/docs/internal/lensintegration"
)

// embeddingModel is the OpenAI-compatible model we ask Lens to use.
// Lens proxies to whichever upstream it has configured; the dim of
// the returned vector must match the page_embeddings.embedding
// column (1536).
const embeddingModel = "text-embedding-3-small"

// similarityThreshold filters out semantic results that are too far
// from the query. 0.75 cosine similarity is the empirical floor for
// "actually relevant" — anything lower drowns out the full-text
// results in the merged ranking.
const similarityThreshold = 0.75

// indexTimeout caps a single IndexPage call so a slow Lens can't
// stall the save-after hook. The actual save is already complete by
// the time this runs (we're in a detached goroutine), but a wedged
// goroutine still wastes a slot.
const indexTimeout = 10 * time.Second

type SemanticResult struct {
	PageID     string  `json:"page_id"`
	Similarity float64 `json:"similarity"`
}

// pgxDB lets the tests pass pgxmock in place of a real pool.
type pgxDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type SemanticSearch struct {
	lensClient *lensintegration.Client
	pool       pgxDB
	httpClient *http.Client
	lensURL    string
	apiKey     string
}

func New(lensClient *lensintegration.Client, pool *pgxpool.Pool) *SemanticSearch {
	var db pgxDB
	if pool != nil {
		db = pool
	}
	return newSemanticSearch(lensClient, db)
}

// newSemanticSearch is the testable constructor — it accepts the
// narrow pgxDB interface so pgxmock pools can be injected directly.
func newSemanticSearch(lensClient *lensintegration.Client, pool pgxDB) *SemanticSearch {
	return &SemanticSearch{
		lensClient: lensClient,
		pool:       pool,
		httpClient: &http.Client{Timeout: indexTimeout},
		// The Lens client doesn't currently expose its URL/key, but
		// we re-read them here for the embeddings endpoint which the
		// client doesn't directly support. SemanticSearch uses the
		// same env vars; main.go passes the resolved values via
		// WithLensCreds.
	}
}

// WithLensCreds wires the URL + API key for the embeddings endpoint.
// Phase 6 chooses to call /v1/proxy/openai/v1/embeddings directly
// rather than extending Client; the Client surface is otherwise
// chat-completion-shaped, and embeddings are a one-off.
func (s *SemanticSearch) WithLensCreds(lensURL, apiKey string) *SemanticSearch {
	s.lensURL = strings.TrimRight(lensURL, "/")
	s.apiKey = apiKey
	return s
}

// IsEnabled reports whether semantic search can be performed. If
// either Lens or the DB is missing, callers should fall back to
// full-text only.
func (s *SemanticSearch) IsEnabled() bool {
	return s.lensClient != nil && s.lensClient.IsConfigured() && s.pool != nil
}

// IndexPage embeds the page text via Lens and upserts the vector
// into page_embeddings. Best-effort: errors are logged but never
// returned, so the save path that calls this from a goroutine never
// has to handle a search-side failure.
func (s *SemanticSearch) IndexPage(ctx context.Context, pageID, _ /*workspaceID*/, text string) error {
	if !s.IsEnabled() {
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	vec, err := s.embed(ctx, pageID, text)
	if err != nil {
		slog.Warn("search: embed failed", slog.String("page_id", pageID), slog.String("err", err.Error()))
		return nil
	}
	if len(vec) == 0 {
		return nil
	}
	encoded := encodeVector(vec)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO page_embeddings (page_id, embedding)
        VALUES ($1, $2::vector)
        ON CONFLICT (page_id) DO UPDATE SET
            embedding  = EXCLUDED.embedding,
            updated_at = NOW()`,
		pageID, encoded,
	)
	if err != nil {
		slog.Warn("search: upsert embedding", slog.String("page_id", pageID), slog.String("err", err.Error()))
		return nil
	}
	return nil
}

// Search returns up to `limit` page IDs whose embeddings are above
// the similarity threshold. Degrades to an empty result on any
// upstream failure so callers can render "no semantic results"
// instead of an error.
func (s *SemanticSearch) Search(ctx context.Context, workspaceID, query string, limit int) ([]SemanticResult, error) {
	if !s.IsEnabled() {
		return []SemanticResult{}, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	vec, err := s.embed(ctx, "query", query)
	if err != nil {
		slog.Warn("search: query embed", slog.String("err", err.Error()))
		return []SemanticResult{}, nil
	}
	if len(vec) == 0 {
		return []SemanticResult{}, nil
	}
	encoded := encodeVector(vec)
	rows, err := s.pool.Query(ctx,
		`SELECT pe.page_id, 1 - (pe.embedding <=> $1::vector) AS similarity
        FROM page_embeddings pe
        JOIN pages p ON p.id = pe.page_id
        WHERE p.workspace_id = $2 AND p.is_template = false
        ORDER BY pe.embedding <=> $1::vector
        LIMIT $3`,
		encoded, workspaceID, limit,
	)
	if err != nil {
		slog.Warn("search: pgvector query", slog.String("err", err.Error()))
		return []SemanticResult{}, nil
	}
	defer rows.Close()
	var out []SemanticResult
	for rows.Next() {
		var r SemanticResult
		if err := rows.Scan(&r.PageID, &r.Similarity); err != nil {
			return []SemanticResult{}, nil
		}
		if r.Similarity < similarityThreshold {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// IndexAllPages walks every non-template page in the workspace and
// indexes them. Boots can call this when the embeddings table is
// empty. Errors are logged per-page and don't abort the batch.
func (s *SemanticSearch) IndexAllPages(ctx context.Context, workspaceID string) error {
	if !s.IsEnabled() {
		return nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, content_text FROM pages
        WHERE workspace_id = $1 AND is_template = false`,
		workspaceID,
	)
	if err != nil {
		return fmt.Errorf("search: list pages: %w", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var (
			id   string
			text string
		)
		if err := rows.Scan(&id, &text); err != nil {
			return err
		}
		_ = s.IndexPage(ctx, id, workspaceID, text)
		n++
	}
	slog.Info("search: backfill done", slog.Int("indexed", n), slog.String("workspace", workspaceID))
	return rows.Err()
}

// ─── Lens embeddings call ────────────────────────────────

// embed asks Lens for a vector embedding of the given text. The
// pageID is passed through purely for the workspace header so Lens
// can attribute cost; the embedding itself is a function of the
// text alone.
func (s *SemanticSearch) embed(ctx context.Context, _, text string) ([]float64, error) {
	if s.lensURL == "" || s.apiKey == "" {
		return nil, errors.New("search: lens creds missing")
	}
	body := map[string]any{
		"model": embeddingModel,
		"input": text,
	}
	enc, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.lensURL+"/v1/proxy/openai/v1/embeddings", bytes.NewReader(enc))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("X-Talyvor-Feature", "docs-search")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("search: lens %s", resp.Status)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	return out.Data[0].Embedding, nil
}

// encodeVector formats a []float64 as the literal pgvector accepts
// when cast via ::vector. Avoids the dependency on the pgvector-go
// driver — embeddings only flow one direction (write), so a string
// literal is enough.
func encodeVector(v []float64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(x, 'f', -1, 64))
	}
	b.WriteByte(']')
	return b.String()
}
