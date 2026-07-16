// Package lenscreds mints and caches per-workspace Lens access tokens.
//
// Docs calls Lens with ONE shared admin key (LENS_API_KEY). A verified Lens recon showed
// that for the admin key the resolved workspace is EMPTY, so every Docs tenant collapses
// into (a) one shared rate-limit bucket (a cross-tenant 429 outage) and (b) the "default"
// spend bucket (per-tenant COGS attribution is blind). The Lens side is already fixed and
// proven (872f676): a per-workspace JWT gets its OWN rate-limit bucket AND attributes spend
// to its real workspace, and a forged workspace claim is rejected.
//
// This provider is the Docs half: given a workspaceID it returns a per-workspace bearer,
// minting one via POST /v1/auth/token with the admin key when absent or near expiry, and
// CACHING it per workspace with refresh-before-expiry. Minting on every call would defeat
// the page-save throttle, so the cache is the point. The token itself is OPAQUE to Docs —
// Lens validates the claim; Docs only carries the string and tracks its expiry.
//
// The admin key stays the MINTING credential ONLY. It must never be sent on a data-path
// request after this — callers use TokenFor for the bearer, never the raw admin key.
package lenscreds

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	defaultTTL     = time.Hour
	defaultSkew    = 5 * time.Minute
	defaultTimeout = 10 * time.Second
	mintPath       = "/v1/auth/token"
)

// Provider mints and caches per-workspace Lens tokens. Safe for concurrent use.
type Provider struct {
	lensURL  string
	adminKey string
	ttl      time.Duration
	skew     time.Duration
	now      func() time.Time
	http     *http.Client

	mu    sync.Mutex // guards the cache map's shape (get-or-create of entries)
	cache map[string]*entry
}

// entry is one workspace's cached token. Its own mutex serializes minting FOR THIS
// workspace so racing callers coalesce onto a single mint, while different workspaces mint
// concurrently (the map mutex is held only to locate the entry, never across the HTTP call).
type entry struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// Options configures a Provider. Zero values fall back to defaults in New.
type Options struct {
	TTL  time.Duration    // token lifetime to request (mapped to ttl_hours); default 1h
	Skew time.Duration    // refresh this long before expiry; default 5m
	Now  func() time.Time // injectable clock (tests); default time.Now
	HTTP *http.Client     // injectable client (tests); default 10s-timeout client
}

// New builds a Provider that mints against lensURL using adminKey.
func New(lensURL, adminKey string, o Options) *Provider {
	if o.TTL <= 0 {
		o.TTL = defaultTTL
	}
	if o.Skew <= 0 {
		o.Skew = defaultSkew
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: defaultTimeout}
	}
	return &Provider{
		lensURL:  lensURL,
		adminKey: adminKey,
		ttl:      o.TTL,
		skew:     o.Skew,
		now:      o.Now,
		http:     o.HTTP,
		cache:    map[string]*entry{},
	}
}

// entryFor returns the cache entry for workspaceID, creating it on first use. The map mutex
// is held only for the lookup/insert, never across a mint.
func (p *Provider) entryFor(workspaceID string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.cache[workspaceID]
	if e == nil {
		e = &entry{}
		p.cache[workspaceID] = e
	}
	return e
}

// TokenFor returns a valid per-workspace bearer token for workspaceID, minting + caching as
// needed. It NEVER returns the admin key: on a mint failure it returns an error and the
// caller decides the fail policy (async index = best-effort/retry; sync search = fail-closed).
func (p *Provider) TokenFor(ctx context.Context, workspaceID string) (string, error) {
	e := p.entryFor(workspaceID)
	e.mu.Lock()
	defer e.mu.Unlock()

	// Cache hit: a token exists and is not yet within `skew` of expiry.
	if e.token != "" && p.now().Before(e.expiresAt.Add(-p.skew)) {
		return e.token, nil
	}
	tok, exp, err := p.mint(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	e.token = tok
	e.expiresAt = exp
	return tok, nil
}

// mint calls Lens's POST /v1/auth/token with the admin key and returns the fresh token and
// its expiry. The admin key is used HERE ONLY — never on a data-path request.
func (p *Provider) mint(ctx context.Context, workspaceID string) (string, time.Time, error) {
	hours := int(p.ttl / time.Hour)
	if hours < 1 {
		hours = 1
	}
	enc, err := json.Marshal(map[string]any{"workspace_id": workspaceID, "ttl_hours": hours})
	if err != nil {
		return "", time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.lensURL+mintPath, bytes.NewReader(enc))
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.adminKey)

	resp, err := p.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", time.Time{}, fmt.Errorf("lenscreds: mint for %q: %s: %s", workspaceID, resp.Status, bytes.TrimSpace(body))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("lenscreds: decode mint response: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("lenscreds: mint for %q returned an empty token", workspaceID)
	}
	// Prefer the server's stated expiry; fall back to now+ttl if it is missing/unparseable so
	// a lenient Lens response still yields a bounded cache lifetime rather than a token we
	// treat as immortal.
	exp := p.now().Add(p.ttl)
	if out.ExpiresAt != "" {
		if parsed, perr := time.Parse(time.RFC3339, out.ExpiresAt); perr == nil {
			exp = parsed
		}
	}
	return out.Token, exp, nil
}
