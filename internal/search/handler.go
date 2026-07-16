package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/docs/internal/authz"
	"github.com/talyvor/docs/internal/page"
	"github.com/talyvor/docs/internal/ratelimit"
)

// fullTextSearcher is the page-store dependency the handler needs.
// Narrow on purpose so this package never grows mutation-side
// privileges over pages.
type fullTextSearcher interface {
	SearchWithRank(ctx context.Context, workspaceID, query string, spaceID *string, limit, offset int) ([]page.SearchResult, error)
}

type Handler struct {
	pages    fullTextSearcher
	semantic *SemanticSearch
	// limit throttles the SEMANTIC side's Lens spend per verified workspace. nil =
	// unthrottled (tests mount bare); main.go always wires it.
	limit *ratelimit.Limiter
}

// WithRateLimit attaches the per-workspace limiter. This route embeds the query via Lens on
// every semantic search (embed(ctx, "query", q)), so it spends per call. It is sized far
// more generously than the AI routes: the frontend debounces at 300ms and type=all is the
// default, so a single person typing drives ~200 embeddings/min — an AI-sized ceiling would
// break Cmd+K. See internal/config for the sizing.
func (h *Handler) WithRateLimit(l *ratelimit.Limiter) *Handler {
	h.limit = l
	return h
}

func NewHandler(pages fullTextSearcher, semantic *SemanticSearch) *Handler {
	return &Handler{pages: pages, semantic: semantic}
}

func (h *Handler) Mount(r chi.Router) {
	if h.limit != nil {
		r.With(h.limit.WorkspaceLimit("wsID")).Get("/workspaces/{wsID}/search", h.Search)
		return
	}
	r.Get("/workspaces/{wsID}/search", h.Search)
}

// Result is one row in the unified response. The JSON tags match the
// shape the frontend SearchModal renders. Source flags which signal
// the row came from.
type Result struct {
	PageID     string  `json:"page_id"`
	PageTitle  string  `json:"page_title"`
	SpaceName  string  `json:"space_name"`
	Headline   string  `json:"headline"`
	Rank       float64 `json:"rank,omitempty"`
	Similarity float64 `json:"similarity,omitempty"`
	Source     string  `json:"source"` // "fulltext" | "semantic" | "both"
	URL        string  `json:"url"`
	AICostUSD  float64 `json:"ai_cost_usd,omitempty"`
}

type response struct {
	Results []Result `json:"results"`
	Total   int      `json:"total"`
	Query   string   `json:"query"`
	TookMS  int64    `json:"took_ms"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) Search(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// A4D: {wsID} comes from the URL — authorize it against the caller's verified memberships before
	// searching, or a member of any workspace could read another workspace's document body text.
	wsID := chi.URLParam(r, "wsID") // nosemgrep: docs-no-url-param-workspace-scope -- authorized by AuthorizeWorkspace on the next line, before any store op
	if _, ok := authz.AuthorizeWorkspace(r.Context(), wsID); !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query must be at least 2 characters"})
		return
	}
	kind := r.URL.Query().Get("type")
	if kind == "" {
		kind = "all"
	}
	var spaceID *string
	if sp := r.URL.Query().Get("space_id"); sp != "" {
		spaceID = &sp
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	var (
		ft    []page.SearchResult
		sem   []SemanticResult
		ftEr  error
		semEr error
		wg    sync.WaitGroup
	)
	// Run both queries concurrently when type=all; sequentially when
	// the caller asked for just one. Semantic search has its own
	// graceful-degradation contract so a failure there returns [].
	if kind == "all" || kind == "fulltext" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ft, ftEr = h.pages.SearchWithRank(r.Context(), wsID, q, spaceID, limit, offset)
		}()
	}
	if kind == "all" || kind == "semantic" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 3-second hard cap on the semantic side so a slow Lens
			// doesn't keep the whole request hanging.
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			sem, semEr = h.semantic.Search(ctx, wsID, q, limit)
		}()
	}
	wg.Wait()
	if ftEr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}
	// Semantic search degrades gracefully (empty results) for every failure EXCEPT one it
	// surfaces as an error: a per-workspace token could not be minted (ErrTokenUnavailable).
	// That is fail-closed by design — we error the search rather than fall back to the shared
	// global key, which would silently re-collapse per-tenant rate-limit + spend attribution.
	if semEr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}

	merged := merge(ft, sem)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	writeJSON(w, http.StatusOK, response{
		Results: merged,
		Total:   len(merged),
		Query:   q,
		TookMS:  time.Since(start).Milliseconds(),
	})
}

// merge combines the two result sets, deduplicating by page_id and
// computing a unified score. Pages that appear in BOTH sets are
// boosted with a "both" source. Pure-semantic results get a synthetic
// headline so the UI still has something to render.
func merge(ft []page.SearchResult, sem []SemanticResult) []Result {
	bySemantic := map[string]float64{}
	for _, s := range sem {
		bySemantic[s.PageID] = s.Similarity
	}
	seen := map[string]bool{}

	type scored struct {
		r     Result
		score float64
	}
	var out []scored

	for _, f := range ft {
		pageID := f.Page.ID
		seen[pageID] = true
		simScore := bySemantic[pageID]
		src := "fulltext"
		if simScore > 0 {
			src = "both"
		}
		// Weighted blend: full-text dominates when both fire.
		score := f.Rank * 0.6
		if simScore*0.4 > score {
			score = simScore * 0.4
		}
		out = append(out, scored{
			r: Result{
				PageID:     pageID,
				PageTitle:  f.Page.Title,
				SpaceName:  f.SpaceName,
				Headline:   f.Headline,
				Rank:       f.Rank,
				Similarity: simScore,
				Source:     src,
				URL:        pageURL(f.Page.SpaceID, pageID),
				AICostUSD:  f.Page.AICostUSD,
			},
			score: score,
		})
	}
	// Pure-semantic rows — pages whose full-text index didn't fire
	// for the query but whose embedding cosine is high enough.
	for _, s := range sem {
		if seen[s.PageID] {
			continue
		}
		out = append(out, scored{
			r: Result{
				PageID:     s.PageID,
				Similarity: s.Similarity,
				Source:     "semantic",
				URL:        pageURL("", s.PageID),
			},
			score: s.Similarity * 0.4,
		})
	}

	// Sort by score desc. Insertion-sort over the slice is fine —
	// we cap at limit=50 above so this is at most 50 elements.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].score > out[j-1].score; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	res := make([]Result, len(out))
	for i, s := range out {
		res[i] = s.r
	}
	return res
}

func pageURL(spaceID, pageID string) string {
	if spaceID == "" {
		return fmt.Sprintf("/pages/%s", pageID)
	}
	return fmt.Sprintf("/spaces/%s/pages/%s", spaceID, pageID)
}
