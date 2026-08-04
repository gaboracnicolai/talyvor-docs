package lensintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// PRICING A PAGE'S OWN AI SPEND — the read half.
//
// Docs binds a page to a Lens request id at call time (internal/page/ai_spend.go) but cannot know
// what the call cost: only Lens prices a completion, from a catalog Docs has no copy of. So the
// cost arrives later, from /v1/api/spend/by-request — the same per-request grain talyvor-track
// consumes, and the reason that endpoint exists.
//
// ⚠ WHY NOT PRICE IT LOCALLY. Because a client that estimates cost from hardcoded rates gets it
// wrong: the VS Code extension does exactly that, with a Haiku rate applied to every model, and is
// out by 4x on the default model and 20x on Opus. Docs asks the system that actually billed.

// RequestSpend is one per-request row. Field names mirror talyvor-track's decoder so the two
// consumers of this endpoint cannot drift in how they read it.
type RequestSpend struct {
	RequestID    string  `json:"request_id"`
	Feature      string  `json:"feature"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	TS           string  `json:"ts"`
}

type byRequestPage struct {
	Rows       []RequestSpend `json:"rows"`
	NextCursor string         `json:"next_cursor"`
}

// SpendByRequest walks every per-request spend row for one workspace over the last `days`,
// following the endpoint's cursor to completion.
//
// ⚠ IT REFUSES TO RETURN A PARTIAL WALK. A page that fails mid-cursor returns an error rather
// than the rows gathered so far. The caller prices exactly-once by request id, so a short read
// would not corrupt a total — but it would silently report "these are the costs" when it means
// "these are some of them", and a caller cannot tell the two apart from a slice. Failing is the
// only answer that stays honest, and the next tick re-walks.
func (c *Client) SpendByRequest(ctx context.Context, workspaceID string, days int) ([]RequestSpend, error) {
	if !c.IsConfigured() {
		return nil, errors.New("lens: not configured")
	}
	if c.tokens == nil {
		return nil, errors.New("lens: no token provider wired")
	}
	if days <= 0 {
		days = 1
	}
	tok, err := c.tokens.TokenFor(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("lens: token for %q: %w", workspaceID, err)
	}

	var out []RequestSpend
	cursor := ""
	// Bounded so a cursor that never terminates cannot spin forever. 200 pages at the endpoint's
	// cap is far beyond any real day of Docs traffic; hitting it means something is wrong, and an
	// error is the honest report.
	for page := 0; page < 200; page++ {
		q := url.Values{}
		q.Set("days", fmt.Sprint(days))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, rErr := http.NewRequestWithContext(ctx, http.MethodGet,
			c.lensURL+"/v1/api/spend/by-request?"+q.Encode(), nil)
		if rErr != nil {
			return nil, rErr
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Talyvor-Workspace", workspaceID)

		resp, dErr := c.httpClient.Do(req)
		if dErr != nil {
			return nil, dErr
		}
		var body byRequestPage
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		status := resp.StatusCode
		_ = resp.Body.Close()
		if status >= 400 {
			return nil, fmt.Errorf("lens: spend by-request: %s", resp.Status)
		}
		if decErr != nil {
			return nil, fmt.Errorf("lens: decode spend by-request: %w", decErr)
		}
		out = append(out, body.Rows...)
		if body.NextCursor == "" {
			return out, nil
		}
		cursor = body.NextCursor
	}
	return nil, errors.New("lens: spend by-request did not terminate within the page bound")
}
