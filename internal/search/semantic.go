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

// ErrTokenUnavailable means a per-workspace Lens token could not be minted. The two data
// paths diverge on it, by design: the async index path treats it as best-effort (logs and
// re-indexes on the page's next save), the sync search path treats it as fail-closed (errors
// the search). NEITHER falls back to the shared global key — that would silently re-collapse
// per-tenant rate-limit + spend attribution, the exact bug this seam fixes.
var ErrTokenUnavailable = errors.New("search: per-workspace lens token unavailable")

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
	tokens     tokenProvider
}

// tokenProvider yields a per-workspace Lens bearer. internal/lenscreds.Provider satisfies
// it. The data-path embed uses this instead of the shared global key so Lens meters + rate-
// limits per workspace (the global key resolves to an empty workspace — see internal/lenscreds).
type tokenProvider interface {
	TokenFor(ctx context.Context, workspaceID string) (string, error)
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
		// The Lens client doesn't expose its URL, so main.go re-passes it via WithLensURL for
		// the embeddings endpoint the client doesn't directly support. The data-path bearer
		// comes from the per-workspace provider wired by WithTokenProvider.
	}
}

// WithLensURL wires the base URL for the embeddings endpoint. Phase 6 chooses to call
// /v1/proxy/openai/v1/embeddings directly rather than extending Client; the Client surface is
// otherwise chat-completion-shaped, and embeddings are a one-off. The data-path CREDENTIAL is
// no longer taken here — it is a per-workspace JWT from WithTokenProvider.
func (s *SemanticSearch) WithLensURL(lensURL string) *SemanticSearch {
	s.lensURL = strings.TrimRight(lensURL, "/")
	return s
}

// WithTokenProvider wires the per-workspace JWT provider. Once set, the embeddings data path
// sends a per-workspace bearer instead of the shared global key. main.go always wires this.
func (s *SemanticSearch) WithTokenProvider(tp tokenProvider) *SemanticSearch {
	s.tokens = tp
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
func (s *SemanticSearch) IndexPage(ctx context.Context, pageID, workspaceID, text string) error {
	if !s.IsEnabled() {
		return nil
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	vec, err := s.embed(ctx, workspaceID, text)
	if err != nil {
		// Best-effort, INCLUDING a mint failure (ErrTokenUnavailable): log and return nil. We
		// never fall back to the global key. The page re-indexes on its next save — the
		// pageindex throttle re-enqueues on the next Update — so the async path "retries" via
		// the normal save loop rather than a bespoke retry here (the throttle owns never-drop).
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
	vec, err := s.embed(ctx, workspaceID, query)
	if err != nil {
		if errors.Is(err, ErrTokenUnavailable) {
			// Fail-closed: a per-workspace token could not be minted. Surface the error so the
			// handler errors the search rather than degrading — and never fall back to the
			// global key, which would re-collapse per-tenant attribution.
			return nil, err
		}
		// Any other embed failure keeps the existing graceful-degradation contract (Lens down
		// ⇒ empty semantic results, full-text still serves).
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

// embed asks Lens for a vector embedding of the given text, attributed to workspaceID. The
// bearer is a PER-WORKSPACE JWT minted for workspaceID — Lens meters + rate-limits off the
// token's claim, so this is how per-tenant attribution actually lands (the X-Talyvor-Workspace
// header is a label Lens ignores for metering). The embedding itself is a function of the text
// alone. On a mint failure this returns ErrTokenUnavailable WITHOUT sending a request — the
// shared global key is never used as the data-path bearer.
func (s *SemanticSearch) embed(ctx context.Context, workspaceID, text string) ([]float64, error) {
	if s.lensURL == "" {
		return nil, errors.New("search: lens url missing")
	}
	if s.tokens == nil {
		return nil, errors.New("search: no token provider wired")
	}
	tok, err := s.tokens.TokenFor(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTokenUnavailable, err)
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
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Talyvor-Feature", "docs-search")
	req.Header.Set("X-Talyvor-Workspace", workspaceID) // observability label; the JWT is authoritative

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
