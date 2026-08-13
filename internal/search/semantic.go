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

// SemanticResult is one vector hit. SpaceID is here because a URL WITHOUT IT IS NOT A URL THIS
// PRODUCT CAN OPEN: the SPA registers `/spaces/:spaceID/pages/:pageID` and nothing else that
// reaches a page, so merge() building `/pages/{id}` sent every semantic-only hit to NotFoundView.
//
// ⚠ IT COSTS NOTHING, AND THAT IS THE POINT — the query below ALREADY joins `pages p` and ALREADY
// filters `p.space_id`. The column was in the join and missing from the SELECT list, so the fix is
// one identifier and not the page read the handler's own comment assumed it would need.
//
// ⚠ Title AND SpaceName ARE HERE FOR THE SAME REASON ONE FIELD LATER: A ROW YOU CAN OPEN AND
// CANNOT READ IS STILL NOT A RESULT. `SearchModal.tsx` draws a hit's identity as exactly
// `{space_name} · {page_title}`, and merge() filled neither on a pure-semantic row, so the reader
// was offered a line with nothing written on it. `title` is another column of the SAME already-
// joined `pages` row; `space_name` is one more join — the IDENTICAL join the full-text half
// already performs (`page.Store.SearchWithRank`: `JOIN spaces sp ON sp.id = p.space_id`) — and it
// cannot drop a hit, because `0002_pages.sql:13` declares `space_id TEXT NOT NULL REFERENCES
// spaces(id)`. `headline` and the three costs are still absent, deliberately; see the note on
// search.Result for why each.
type SemanticResult struct {
	PageID     string  `json:"page_id"`
	SpaceID    string  `json:"space_id"`
	Title      string  `json:"title"`
	SpaceName  string  `json:"space_name"`
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
	// AN EMPTIED DOCUMENT LEAVES THE INDEX. It does NOT keep the last vector it had.
	//
	// This branch used to `return nil` on its own, and the cost half of that is right — there is
	// nothing to embed in an empty string, so do not pay Lens for it. What it also decided,
	// silently, was that the STORED vector stays: `page_embeddings` is an upsert keyed on page_id
	// with no other writer, so skipping the write leaves the previous text's embedding on disk and
	// the page goes on answering semantic queries for content it no longer holds. The full-text
	// half stops matching in the same save — its index is a derived expression over the live
	// `content_text` — so the two halves of one search disagreed about whether the document said
	// anything. Measured through page.Store.Update on real Postgres in
	// staleembedding_realpg_test.go.
	//
	// Best-effort and logged, exactly like the upsert below: this runs in a detached goroutine
	// behind the editor's save, so a failure here must not fail a save the user already committed
	// — but it must not be invisible either. The next save of this page re-runs the hook
	// (pageindex.Throttle re-queues on any newer content), so a lost retirement is retried by the
	// ordinary save loop rather than by a bespoke retry here.
	if strings.TrimSpace(text) == "" {
		if _, err := s.pool.Exec(ctx,
			`DELETE FROM page_embeddings WHERE page_id = $1`, pageID); err != nil {
			slog.Warn("search: retire embedding", slog.String("page_id", pageID), slog.String("err", err.Error()))
		}
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
//
// spaceID SCOPES THE SEARCH TO ONE SPACE, and it is a parameter rather than a caller-side filter
// because this half had no way to express it at all: the handler took one `space_id`, handed it to
// page.SearchWithRank, and ran this query without it — so "search inside this space" answered with
// pages from every other space in the workspace, on a row shape that carries no space_name to say
// so. nil means the whole workspace, matching SearchWithRank's `$3::text IS NULL` arm.
//
// offset IS THE SAME DEFECT ON THE OTHER PAGINATION PARAMETER, and it was worse than a repeat.
// The handler reads one `offset`; SearchWithRank applied it and this query had no OFFSET at all, so
// EVERY page of a semantic search was page 1 — `offset=4` over four rows still returned rows 1-2,
// which made the semantic tail unreachable at every offset. On type=all — the default when the
// caller names no type — the stale rows then out-ranked and displaced the full-text rows that HAD
// paged correctly. It clamps and bounds exactly as SearchWithRank does, so the two halves cannot
// drift.
//
// ⚠ WHO COULD REACH IT, MEASURED RATHER THAN ASSUMED: `offset` is a query parameter of the public
// HTTP endpoint, and no client in this repo sends one today — SearchModal calls useSearch with no
// options, and the two MCP call sites pass a literal 0. So this was a broken parameter on the API
// surface rather than a broken screen. (useSearch's react-query key omits `offset` while carrying
// every other option, which is the same half-honoured shape one layer up; it is latent for the
// same reason and is NOT changed here.)
//
// ⚠ `, pe.page_id` IS THE WHOLE OF THE PAGING CONTRACT, AND IT IS NOT DECORATION. A distance is
// not a key: two pages with the same text embed to the same vector and sit at the same distance,
// and rows at an equal distance have NO defined relative order. `LIMIT/OFFSET` over an undefined
// order is a question the query cannot answer, so Postgres answers it differently per page.
//
// MEASURED THROUGH THIS FUNCTION, 40 equidistant rows, ONE connection, ONE plan, nothing written
// between the requests: paging 5 at a time served page 0 at offsets 0, 5 AND 10 while pages 5 and
// 10 were never served at all. It is not a race and not the planner — a bounded sort is a TOP-N
// HEAPSORT whose N is `limit + offset`, so every page runs a DIFFERENT sort and a different N
// permutes an all-equal input differently. Paging is the thing that changes N.
//
// The planner is a SECOND way to get the same wrong answer (six identical embeddings ordered
// [one two three four five six] under a seq scan and [one four two three five six] under a
// pages_pkey index scan), and `page_id` — the table's primary key — closes both by construction:
// a total order is a total order under every plan. The full-text half carries the same fix
// (`, p.id` in page.SearchWithRank). Guarded by semanticpaging_realpg_test.go, which asserts the
// product property — every row served exactly once — and never pins a plan.
func (s *SemanticSearch) Search(ctx context.Context, workspaceID, query string, spaceID *string, limit, offset int) ([]SemanticResult, error) {
	if !s.IsEnabled() {
		return []SemanticResult{}, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
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
		// `sp` is an INNER join and that is safe rather than convenient: pages.space_id is
		// `TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE` (0002_pages.sql:13), so every
		// row this statement can reach has exactly one spaces row. It is the same join
		// page.Store.SearchWithRank uses for the same column on the full-text half.
		`SELECT pe.page_id, p.space_id, p.title, sp.name AS space_name,
               1 - (pe.embedding <=> $1::vector) AS similarity
        FROM page_embeddings pe
        JOIN pages p ON p.id = pe.page_id
        JOIN spaces sp ON sp.id = p.space_id
        WHERE p.workspace_id = $2 AND p.is_template = false
          AND ($4::text IS NULL OR p.space_id = $4)
        ORDER BY pe.embedding <=> $1::vector, pe.page_id
        LIMIT $3 OFFSET $5`,
		encoded, workspaceID, limit, spaceID, offset,
	)
	if err != nil {
		slog.Warn("search: pgvector query", slog.String("err", err.Error()))
		return []SemanticResult{}, nil
	}
	defer rows.Close()
	var out []SemanticResult
	for rows.Next() {
		var r SemanticResult
		if err := rows.Scan(&r.PageID, &r.SpaceID, &r.Title, &r.SpaceName, &r.Similarity); err != nil {
			// A SELECT list and a Scan that disagree is a PROGRAMMING error, and it used to be
			// laundered through the same silent path as an infrastructure one — no error and,
			// unlike every other failure in this method, no log either. It stays a degradation
			// (see the contract above), but it is no longer invisible.
			slog.Warn("search: pgvector scan", slog.String("err", err.Error()))
			return []SemanticResult{}, nil
		}
		if r.Similarity < similarityThreshold {
			continue
		}
		out = append(out, r)
	}
	// ⚠ rows.Err() IS A THIRD DOOR, NOT A TIDY-UP, AND IT IS THE ONE THE REALISTIC FAILURE USES.
	// pgx reports an error raised DURING row production here and NOWHERE ELSE: `Query()` above
	// returns nil, `rows.Next()` returns false on its first call, and the Scan branch is never
	// reached. Which door an error arrives at is decided by whether it was raised before or
	// after the row description — a pgx implementation detail, not a product distinction — so
	// this must behave exactly as the `Query()` failure ten lines up already does.
	//
	// MEASURED on real Postgres through this function, three indexed pages seeded: a query
	// vector of the wrong DIMENSION (`different vector dimensions 1536 and 3`, SQLSTATE 22000)
	// returned ([], nil) with an EMPTY log. The dimension is a property of the model Lens is
	// configured with, not of this schema — `embedding` is `vector(1536)` but `$1::vector` is
	// unconstrained — so an upstream model change turns the semantic half off across every
	// workspace, permanently and silently, while full-text keeps answering 200.
	//
	// ⚠ `out` IS DISCARDED RATHER THAN RETURNED. A truncated result set handed back with a nil
	// error is an incomplete answer presented as a complete one, which is the same silence one
	// layer up. Guarded by semanticsilence_realpg_test.go and semanticscan_test.go.
	if err := rows.Err(); err != nil {
		slog.Warn("search: pgvector rows", slog.String("err", err.Error()))
		return []SemanticResult{}, nil
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
