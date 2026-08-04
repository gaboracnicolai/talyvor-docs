// Package lensintegration is the thin HTTP client that fronts every
// AI call Docs makes. All inference flows through Lens — never
// directly to Anthropic or OpenAI — so usage rolls up into a single
// place for billing, rate-limiting and observability. The Lens
// integration is opt-in: an empty DOCS_LENS_URL makes IsConfigured
// return false and the AI engine surfaces a friendly "AI unavailable"
// state instead of erroring.
package lensintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Timeout caps every individual Lens round-trip. Generations longer
// than this should stream — Phase 5 hard-caps at synchronous output
// because the editor doesn't yet handle partial chunks.
const defaultTimeout = 30 * time.Second

type Client struct {
	lensURL    string
	apiKey     string
	httpClient *http.Client
	tokens     tokenProvider
}

// tokenProvider yields a per-workspace Lens bearer. internal/lenscreds.Provider satisfies it.
// The completions data path uses this instead of the shared global key so Lens meters + rate-
// limits per workspace — the global key resolves to an empty workspace (see internal/lenscreds).
type tokenProvider interface {
	TokenFor(ctx context.Context, workspaceID string) (string, error)
}

func New(lensURL, apiKey string) *Client {
	return &Client{
		lensURL:    strings.TrimRight(lensURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// WithTokenProvider wires the per-workspace JWT provider. Once set, completions carry a
// per-workspace bearer instead of the shared global key. main.go always wires this; apiKey
// remains the IsConfigured sentinel and is the same admin key the provider mints with.
func (c *Client) WithTokenProvider(tp tokenProvider) *Client {
	c.tokens = tp
	return c
}

// IsConfigured returns true when both URL and API key are set. Every
// AI feature short-circuits to an "unavailable" response when this is
// false.
func (c *Client) IsConfigured() bool {
	return c.lensURL != "" && c.apiKey != ""
}

// Complete forwards an Anthropic-shaped chat completion through the
// Lens proxy. Caller-supplied workspaceID flows through as the
// X-Talyvor-Workspace header so Lens can attribute spend.
func (c *Client) Complete(ctx context.Context, workspaceID, prompt, systemPrompt, model string) (string, error) {
	return c.CompleteWithFeature(ctx, workspaceID, prompt, systemPrompt, model, "docs-ai")
}

// CompleteWithFeature is Complete with a customisable feature tag.
// The engine uses this to attribute cost per AI affordance
// ("docs-ai-write", "docs-ai-summarize", ...) so usage dashboards can
// distinguish writing tools from Q&A.
func (c *Client) CompleteWithFeature(ctx context.Context, workspaceID, prompt, systemPrompt, model, feature string) (string, error) {
	if !c.IsConfigured() {
		return "", errors.New("lens: not configured")
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": 2048,
		"system":     systemPrompt,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	raw, err := c.post(ctx, "/v1/proxy/anthropic/v1/messages", workspaceID, feature, body)
	if err != nil {
		return "", err
	}
	return parseAnthropic(raw)
}

// CompleteOpenAI is the OpenAI-shaped sibling of Complete. Phase 5
// keeps both shapes alive so Lens can route to either upstream
// without Docs needing to know which one is cheaper today.
func (c *Client) CompleteOpenAI(ctx context.Context, workspaceID, prompt, systemPrompt, model string) (string, error) {
	if !c.IsConfigured() {
		return "", errors.New("lens: not configured")
	}
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": prompt},
		},
	}
	raw, err := c.post(ctx, "/v1/proxy/openai/v1/chat/completions", workspaceID, "docs-ai", body)
	if err != nil {
		return "", err
	}
	return parseOpenAI(raw)
}

// CompleteWithRequestID is CompleteWithFeature plus Lens's request id for the call.
//
// ⚠ THE REQUEST ID IS THE ATTRIBUTION KEY, and this is why the Track-style mechanism does not
// transfer. Track binds spend to an issue by sending the issue's human IDENTIFIER as
// X-Talyvor-Feature and matching it back with `WHERE identifier = feature`. A page has no such
// identifier: its id is a UUID (unreadable in a feature column, and it would blow up the
// cardinality of Lens's by-feature aggregation — one feature value per page rather than per
// operation), and its slug is MUTABLE, so a rename would orphan every cost recorded under the old
// one. Lens instead returns X-Talyvor-Request-ID on every proxied completion and serves
// /v1/api/spend/by-request keyed by the same value — so Docs binds page↔request at the moment it
// makes the call, and prices it later. The binding is recorded by the process that observed it
// rather than reconstructed from a string, and it survives a rename.
//
// The feature header is deliberately LEFT as the operation ("docs-ai-write"). Making it
// page-scoped would give Lens's own dashboards a per-page dimension at the cost of making
// by-feature aggregation useless, and Docs would still not be able to read it back — Lens has no
// by-feature-prefix query. A page-scoped feature would be a header nothing reads.
func (c *Client) CompleteWithRequestID(ctx context.Context, workspaceID, prompt, systemPrompt, model, feature string) (text, requestID string, err error) {
	if !c.IsConfigured() {
		return "", "", errors.New("lens: not configured")
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": 2048,
		"system":     systemPrompt,
		"messages": []map[string]any{
			{"role": "user", "content": prompt},
		},
	}
	raw, reqID, err := c.postWithRequestID(ctx, "/v1/proxy/anthropic/v1/messages", workspaceID, feature, body)
	if err != nil {
		return "", "", err
	}
	out, err := parseAnthropic(raw)
	return out, reqID, err
}

func (c *Client) post(ctx context.Context, path, workspaceID, feature string, body map[string]any) ([]byte, error) {
	raw, _, err := c.postWithRequestID(ctx, path, workspaceID, feature, body)
	return raw, err
}

func (c *Client) postWithRequestID(ctx context.Context, path, workspaceID, feature string, body map[string]any) ([]byte, string, error) {
	enc, err := json.Marshal(body)
	if err != nil {
		return nil, "", err
	}
	// Per-workspace bearer — Lens meters + rate-limits off THIS token's claim. The shared
	// global key (c.apiKey) is the MINTING credential only; it is never sent here. On a mint
	// failure we error the completion (fail-closed) rather than fall back to the global key,
	// which would silently re-collapse per-tenant rate-limit + spend attribution.
	if c.tokens == nil {
		return nil, "", errors.New("lens: no token provider wired")
	}
	tok, err := c.tokens.TokenFor(ctx, workspaceID)
	if err != nil {
		return nil, "", fmt.Errorf("lens: token for %q: %w", workspaceID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.lensURL+path, bytes.NewReader(enc))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Talyvor-Feature", feature)
	req.Header.Set("X-Talyvor-Workspace", workspaceID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("lens: %s", resp.Status)
	}
	// Lens stamps X-Talyvor-Request-ID on every proxied completion (talyvor-lens
	// internal/proxy/proxy.go). It is the key /v1/api/spend/by-request is served by, so it is what
	// a page↔cost binding is recorded against. Absent (an older Lens) ⇒ empty, and the caller
	// simply records no binding rather than inventing one.
	reqID := resp.Header.Get("X-Talyvor-Request-ID")
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return raw, reqID, nil
}

// parseAnthropic pulls the assistant text out of an Anthropic Messages
// API response. Lens proxies the wire format unchanged, so we deal
// with the same `content[].text` shape Anthropic returns directly.
func parseAnthropic(raw []byte) (string, error) {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("lens: decode anthropic: %w", err)
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), nil
}

// parseOpenAI extracts the first choice's message content from an
// OpenAI chat completion response.
func parseOpenAI(raw []byte) (string, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("lens: decode openai: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", nil
	}
	return resp.Choices[0].Message.Content, nil
}
