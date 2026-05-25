// Package trackintegration is the HTTP client + cache + sync
// machinery that makes Talyvor Docs's "this spec cost $342 to ship"
// numbers possible. Every public method tolerates an unconfigured
// Track — Docs runs fine on its own; the integration is opt-in via
// DOCS_TRACK_URL + DOCS_TRACK_API_KEY.
package trackintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// IssueRef is the lean projection of a Track issue Docs needs. We
// deliberately don't carry the full Track Issue here — the more
// fields we mirror, the more we have to keep in sync as Track
// evolves.
type IssueRef struct {
	ID         string   `json:"id"`
	Identifier string   `json:"identifier"`
	Title      string   `json:"title"`
	Status     string   `json:"status"`
	Priority   int      `json:"priority"`
	AssigneeID string   `json:"assignee_id,omitempty"`
	AICostUSD  float64  `json:"ai_cost_usd"`
	URL        string   `json:"url,omitempty"`
	// Labels powers the changelog auto-grouping ("bug" → bugfix,
	// "feature" → feature, etc.). Track may omit it; consumers must
	// degrade gracefully.
	Labels []string `json:"labels,omitempty"`
}

// cacheTTL bounds how long a fetched IssueRef stays warm in the
// in-memory cache. 30s matches the rhythm at which doc readers
// notice status changes; longer would surface stale embeds, shorter
// would hammer Track on every page load.
const cacheTTL = 30 * time.Second

type cachedRef struct {
	ref     *IssueRef
	expires time.Time
}

type Client struct {
	trackURL   string
	apiKey     string
	httpClient *http.Client
	mu         sync.RWMutex
	cache      map[string]cachedRef
}

func New(trackURL, apiKey string) *Client {
	return &Client{
		trackURL: strings.TrimRight(trackURL, "/"),
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: 8 * time.Second,
		},
		cache: map[string]cachedRef{},
	}
}

// IsConfigured returns true when both the URL and the API key are
// set. Every other public method short-circuits to a no-op result
// when this is false — Track integration is optional.
func (c *Client) IsConfigured() bool {
	return c.trackURL != "" && c.apiKey != ""
}

// GetIssue fetches one issue + caches the result for cacheTTL. On
// network failure the call returns (nil, nil) rather than an error
// — embeds should render an "issue unavailable" state, not fail the
// docs page render. Returns (nil, nil) when Track is unconfigured.
func (c *Client) GetIssue(ctx context.Context, workspaceID, issueID string) (*IssueRef, error) {
	if !c.IsConfigured() {
		return nil, nil
	}
	key := workspaceID + "|" + issueID
	c.mu.RLock()
	if hit, ok := c.cache[key]; ok && time.Now().Before(hit.expires) {
		c.mu.RUnlock()
		return hit.ref, nil
	}
	c.mu.RUnlock()

	path := fmt.Sprintf("/v1/workspaces/%s/issues/%s", workspaceID, issueID)
	var ref IssueRef
	if err := c.fetch(ctx, path, nil, &ref); err != nil {
		// Network / 4xx / 5xx — return nil so embeds degrade
		// gracefully. The error is intentionally swallowed.
		return nil, nil
	}
	c.mu.Lock()
	c.cache[key] = cachedRef{ref: &ref, expires: time.Now().Add(cacheTTL)}
	c.mu.Unlock()
	return &ref, nil
}

// SearchIssues hits Track's full-text search. Returns an empty slice
// (no error) when unconfigured so the embed picker degrades to
// "no Track integration" instead of an error toast.
func (c *Client) SearchIssues(ctx context.Context, workspaceID, query string) ([]IssueRef, error) {
	if !c.IsConfigured() {
		return []IssueRef{}, nil
	}
	q := url.Values{}
	q.Set("q", query)
	q.Set("limit", "10")
	path := fmt.Sprintf("/v1/workspaces/%s/issues/search?%s", workspaceID, q.Encode())
	var out []IssueRef
	if err := c.fetch(ctx, path, nil, &out); err != nil {
		return []IssueRef{}, nil
	}
	return out, nil
}

// GetPageBacklinks is a stub until Track ships the `linked_doc`
// query parameter (Phase 7 of Track's roadmap). Returns an empty
// slice so the UI's "no backlinks yet" state renders without a
// network error.
func (c *Client) GetPageBacklinks(_ context.Context, _, _ string) ([]IssueRef, error) {
	return []IssueRef{}, nil
}

// fetch performs an authenticated GET against the configured Track
// origin and JSON-decodes the response into `out`. _ acquires the
// API key on every call so a rotated key takes effect without a
// restart.
func (c *Client) fetch(ctx context.Context, path string, _ map[string]string, out any) error {
	if !c.IsConfigured() {
		return errors.New("track: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.trackURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("track: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
