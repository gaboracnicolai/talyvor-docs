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

// pageReader authorizes a READ of ONE page's content for the verified caller.
// *spaceauth.Authorizer satisfies it — deliberately, because that is the shipped primitive that
// already composes the page+space meta join with permission.CheckPage. Search introduces NO new
// access model; it asks the same engine every other read surface asks.
type pageReader interface {
	AuthorizePageRead(ctx context.Context, pageID string) (found, canView bool)
}

// maxFetchFactor / maxFetchRows size the window the store is asked for when an access gate is
// wired, so rows the caller may not read do not turn into rows they never see.
//
// ⚠ WHAT IS STILL NOT GUARANTEED, STATED SO THE NEXT PERSON DOES NOT HAVE TO REDISCOVER IT: a page
// is short whenever more than (maxFetchFactor-1)×limit consecutive HIDDEN rows out-rank the next
// visible one — a workspace that is mostly private spaces can still under-fill. Closing that
// completely means either the visibility predicate in SQL (a SECOND writer of the access rule,
// which is the class of defect #73 was) or paging by a cursor this endpoint does not have.
// `total` has always been len(results) rather than a corpus count, so nothing here newly
// misreports a total.
//
// maxFetchRows is the store's own ceiling (SearchWithRank clamps to 50), named here so the two
// numbers cannot silently disagree.
const (
	maxFetchFactor = 4
	maxFetchRows   = 50
)

type Handler struct {
	pages    fullTextSearcher
	semantic *SemanticSearch
	// limit throttles the SEMANTIC side's Lens spend per verified workspace. nil =
	// unthrottled (tests mount bare); main.go always wires it.
	limit *ratelimit.Limiter
	// access drops rows the caller may not VIEW. nil = unfiltered — the same convention `limit`
	// uses so the many tests that mount a bare handler keep exercising the merge/rank logic
	// without a database behind them. main.go always wires it, and mainwiring_test.go is the
	// tripwire on that line being deleted.
	access pageReader
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

// WithAccess attaches the per-page read gate. THE QUERY IS THE TARGET HERE, NOT AN ID, so no chi
// URL-param resolver (permission.RequireAccess) can reach this route — which is why search was the
// one read surface in the product that authorized the WORKSPACE and stopped. A workspace member
// with no grant on a PRIVATE space received that space's page titles, its name, and a ts_headline
// EXCERPT OF THE BODY. See privatespace_realpg_test.go for the measurement.
func (h *Handler) WithAccess(a pageReader) *Handler {
	h.access = a
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

	// THE THREE COST FIELDS ARE THE PAGE JSON'S THREE, NOT A CHOSEN ONE OF THEM. A page has two
	// costs and migration 0018 exists because conflating them was the defect: ai_cost_usd is the
	// cost of the Track ISSUES linked to the page (overwritten by a sweep), own_ai_cost_usd is the
	// cost of AI operations performed ON the document (accumulated), and the total is their sum.
	// This row used to carry ai_cost_usd alone — precisely the half 0018 was written because it
	// was missing — so a document whose entire spend was its own AI work reported nothing.
	//
	// ⚠ POINTERS, AND NOT AS A STYLE CHOICE. A row has three possible states and a bare float can
	// only express two. `omitempty` on a float64 deletes the field when the value is 0, so
	// "this page cost nothing" and "this surface did not report a cost" were the same bytes. They
	// are different facts: a SEMANTIC-ONLY row is built from a vector hit with no pages row read,
	// so no cost is KNOWN for it and emitting 0.0 would be a fabricated zero. nil means not
	// reported; 0 means measured and zero. `omitempty` on a pointer omits only nil.
	AICostUSD      *float64 `json:"ai_cost_usd,omitempty"`
	OwnAICostUSD   *float64 `json:"own_ai_cost_usd,omitempty"`
	TotalAICostUSD *float64 `json:"total_ai_cost_usd,omitempty"`
}

// ptr returns a pointer to v. Used only for the cost fields, whose nil/zero distinction is
// load-bearing — see the note on Result.
func ptr(v float64) *float64 { return &v }

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

	// OVER-FETCH, BECAUSE THE ROWS THE CALLER MAY NOT READ ARE DROPPED AFTER THE SQL LIMIT.
	//
	// Measured: with limit=1 and one hidden page out-ranking a readable one, the store returns the
	// hidden row alone, the access gate drops it, and the caller is told NOTHING MATCHED for a
	// document they may open. Asking for a wider window makes that the exception rather than the
	// rule. It is a mitigation, not a guarantee, and the residual case is stated on maxFetchFactor.
	fetchLimit := limit * maxFetchFactor
	if fetchLimit > maxFetchRows {
		fetchLimit = maxFetchRows
	}
	if h.access == nil {
		fetchLimit = limit
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
			ft, ftEr = h.pages.SearchWithRank(r.Context(), wsID, q, spaceID, fetchLimit, offset)
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
			// fetchLimit AND offset, the same pair the full-text half receives above. The
			// semantic query took no offset at all, so every page of a semantic search was
			// page 1 — see the note on SemanticSearch.Search.
			sem, semEr = h.semantic.Search(ctx, wsID, q, spaceID, fetchLimit, offset)
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

	// Drop rows the caller may not VIEW, BEFORE the limit truncation — a page they cannot read
	// must not consume one of their result slots.
	merged := h.visibleTo(r.Context(), merge(ft, sem))
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

// visibleTo drops every row whose page the caller may not read, asking the same engine the REST
// page routes ask (one AuthorizePageRead per row, at most `limit` ≤ 50 of them).
//
// ⚠ IT APPLIES TO BOTH HALVES, AND THE SEMANTIC HALF IS NOT THE SMALLER ONE. A semantic-only row
// carries no title and no headline, so the leak there is narrower — a page id, a URL and a cosine
// similarity — but "this private document is about what you just asked" is still an answer about a
// document the caller cannot open, and it comes from the same unfiltered pool.
//
// ⚠ A FILTERED PAGE IS A SHORT PAGE, SAID PLAINLY. The LIMIT/OFFSET ran in SQL before this, so a
// caller asking for 10 can receive 7 while more visible rows exist further down. Fixing that means
// over-fetching and re-paging against a count this endpoint does not have; `total` has always been
// len(results) rather than a corpus count, so nothing here newly misreports a total.
//
// nil access ⇒ unfiltered, and that is why cmd/docs/main.go's wiring has its own guard.
func (h *Handler) visibleTo(ctx context.Context, rows []Result) []Result {
	if h.access == nil {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		found, canView := h.access.AuthorizePageRead(ctx, r.PageID)
		if !found || !canView {
			continue
		}
		out = append(out, r)
	}
	return out
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
				// A pages row WAS read for this hit, so all three costs are known — including
				// when they are zero. See the note on Result for why they are pointers.
				AICostUSD:      ptr(f.Page.AICostUSD),
				OwnAICostUSD:   ptr(f.Page.OwnAICostUSD),
				TotalAICostUSD: ptr(f.Page.TotalAICostUSD),
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
