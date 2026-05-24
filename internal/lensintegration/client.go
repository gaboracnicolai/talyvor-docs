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
}

func New(lensURL, apiKey string) *Client {
	return &Client{
		lensURL:    strings.TrimRight(lensURL, "/"),
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
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

func (c *Client) post(ctx context.Context, path, workspaceID, feature string, body map[string]any) ([]byte, error) {
	enc, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.lensURL+path, bytes.NewReader(enc))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-Talyvor-Feature", feature)
	req.Header.Set("X-Talyvor-Workspace", workspaceID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("lens: %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
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
