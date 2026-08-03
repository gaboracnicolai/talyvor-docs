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

	"github.com/talyvor/docs/internal/membership"
)

// IssueRef is the lean projection of a Track issue Docs needs. We
// deliberately don't carry the full Track Issue here — the more
// fields we mirror, the more we have to keep in sync as Track
// evolves.
type IssueRef struct {
	ID         string  `json:"id"`
	Identifier string  `json:"identifier"`
	Title      string  `json:"title"`
	Status     string  `json:"status"`
	Priority   int     `json:"priority"`
	AssigneeID string  `json:"assignee_id,omitempty"`
	AICostUSD  float64 `json:"ai_cost_usd"`
	URL        string  `json:"url,omitempty"`
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
	trackURL         string
	apiKey           string
	memberSyncSecret string // DEDICATED secret for GET /v1/service/members — NOT apiKey
	httpClient       *http.Client
	mu               sync.RWMutex
	cache            map[string]cachedRef
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

// WithMemberSyncSecret sets the dedicated bearer for the member-sync pull (A0b PR-2).
// Separate from apiKey — never reuse the issue-link key for roster access.
func (c *Client) WithMemberSyncSecret(secret string) *Client {
	c.memberSyncSecret = secret
	return c
}

// MemberSyncConfigured reports whether the member-sync pull can run (both the Track URL
// and the dedicated member-sync secret are set). Unset ⇒ the syncer skips member-sync.
func (c *Client) MemberSyncConfigured() bool {
	return c.trackURL != "" && c.memberSyncSecret != ""
}

// GetWorkspaceMembers pulls one workspace's roster from Track's GET /v1/service/members.
// Authorized by the DEDICATED member-sync secret (never apiKey). Single page, limit=500 —
// rosters are assumed < 500; a workspace exceeding that would need offset pagination
// (deferred, flagged). A non-2xx (esp. 401) is an error; a genuinely-empty roster is
// ([], nil) — the caller (reconcile) treats empty as skip-prune, so a transient error that
// surfaces as an error here never reaches the pruning path.
func (c *Client) GetWorkspaceMembers(ctx context.Context, workspaceID string) ([]membership.MemberRef, error) {
	if !c.MemberSyncConfigured() {
		return nil, errors.New("trackintegration: member sync not configured (need DOCS_TRACK_URL + DOCS_TRACK_MEMBER_SYNC_SECRET)")
	}
	q := url.Values{"workspace_id": {workspaceID}, "limit": {"500"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.trackURL+"/v1/service/members?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.memberSyncSecret)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trackintegration: member pull for %s: %s", workspaceID, resp.Status)
	}
	var out []membership.MemberRef
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("trackintegration: decode members: %w", err)
	}
	return out, nil
}

// IsConfigured returns true when both the URL and the API key are
// set. Every other public method short-circuits to a no-op result
// when this is false — Track integration is optional.
func (c *Client) IsConfigured() bool {
	return c.trackURL != "" && c.apiKey != ""
}

// fetchIssue is the ONE fetch+cache path behind both issue reads below. It reports failure
// honestly; the two exported wrappers decide what to do about it, which is the only thing they
// disagree on. Keeping the fetch single-sourced is what stops the embed path and the money path
// from drifting apart the next time either is touched.
func (c *Client) fetchIssue(ctx context.Context, workspaceID, issueID string) (*IssueRef, error) {
	if !c.IsConfigured() {
		return nil, errors.New("track: not configured")
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
		return nil, err
	}
	c.mu.Lock()
	c.cache[key] = cachedRef{ref: &ref, expires: time.Now().Add(cacheTTL)}
	c.mu.Unlock()
	return &ref, nil
}

// GetIssue fetches one issue + caches the result for cacheTTL. On
// network failure the call returns (nil, nil) rather than an error
// — embeds should render an "issue unavailable" state, not fail the
// docs page render. Returns (nil, nil) when Track is unconfigured.
//
// ⚠ THE SWALLOW IS CORRECT HERE AND ONLY HERE. It is right for a rendered embed and wrong for
// anything that adds numbers up: a caller that treats the nil as "cost nothing" turns an
// outage into a silent write-down. The cost syncer used to do exactly that. Money reads go
// through IssueCost, which surfaces what this deliberately hides.
func (c *Client) GetIssue(ctx context.Context, workspaceID, issueID string) (*IssueRef, error) {
	ref, err := c.fetchIssue(ctx, workspaceID, issueID)
	if err != nil {
		return nil, nil
	}
	return ref, nil
}

// IssueCost reads one issue's AI cost for the MONEY path, and reports every failure as an
// error rather than as a number.
//
// EVERY non-success is an error, 404 included. A missing issue is not an authoritative $0.00:
// the likeliest cause of a 404 in the cost loop is a workspace-scoping mistake — asking about
// tenant B's issue under tenant A's workspace — which is precisely the defect that used to
// write every non-default tenant's pages down to zero. "Not found" and "cost nothing" must not
// be the same value, because the caller sums whatever it gets back.
//
// The cost of this strictness, stated plainly: a genuinely-deleted Track issue that a page is
// still linked to will keep that page's total from refreshing until the link is removed. That
// is a visible, logged stall on a stale-but-once-true number, which is the right trade against
// an invisible write-down to a number that was never true.
func (c *Client) IssueCost(ctx context.Context, workspaceID, issueID string) (float64, error) {
	ref, err := c.fetchIssue(ctx, workspaceID, issueID)
	if err != nil {
		return 0, err
	}
	if ref == nil {
		return 0, fmt.Errorf("trackintegration: issue %q in workspace %q: no issue returned", issueID, workspaceID)
	}
	return ref.AICostUSD, nil
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

// ListWorkspaceIDs pulls every workspace id on the deployment from Track's
// GET /v1/service/workspaces — the same gwExempt, member-sync-secret-gated surface as
// GetWorkspaceMembers above.
//
// Track is the tenancy source of truth (it mints a workspace per identity at login), so this is
// the question Docs actually needs answered: "which workspaces exist", not "which have content".
// See enumerate.go for why that distinction was a deadlock.
func (c *Client) ListWorkspaceIDs(ctx context.Context) ([]string, error) {
	if !c.MemberSyncConfigured() {
		return nil, errors.New("trackintegration: member sync not configured (need DOCS_TRACK_URL + DOCS_TRACK_MEMBER_SYNC_SECRET)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.trackURL+"/v1/service/workspaces", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.memberSyncSecret)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("trackintegration: workspace enumeration: %s", resp.Status)
	}
	var out struct {
		WorkspaceIDs []string `json:"workspace_ids"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("trackintegration: decode workspaces: %w", err)
	}
	// A nil slice and an empty one mean the same thing here — a deployment with no workspaces —
	// and enumerateWorkspaces treats that as an ANSWER, not a failure.
	if out.WorkspaceIDs == nil {
		out.WorkspaceIDs = []string{}
	}
	return out.WorkspaceIDs, nil
}
