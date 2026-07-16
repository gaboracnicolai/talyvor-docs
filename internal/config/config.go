// Package config loads server settings from environment variables.
// Empty values are tolerated for non-critical fields; the only hard
// requirement is DOCS_DATABASE_URL, without which the server can't
// reach Postgres and exits at boot.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

var ErrMissingEnv = errors.New("missing required env var")

type Config struct {
	ListenAddr  string
	DatabaseURL string
	LogLevel    string

	// Cross-product links — populated when this Docs instance lives
	// alongside a Track / Lens deployment. Empty values disable the
	// integration silently.
	TrackURL    string
	TrackAPIKey string
	LensURL     string
	LensAPIKey  string

	// TrackMemberSyncSecret authenticates the member-sync pull from Track's
	// GET /v1/service/members (Authorization: Bearer). It is a DEDICATED secret,
	// SEPARATE from TrackAPIKey (the issue-link integration) — never reuse them.
	// Unset ⇒ member-sync is disabled (the syncer skips it), same posture as an
	// unconfigured cost-sync. Server-side only; never browser-reachable.
	TrackMemberSyncSecret string

	// DefaultWorkspaceID is the tenant the cost syncer operates
	// against. Phase 4 supports a single workspace per Docs
	// instance; Phase 5 will iterate workspaces from the DB.
	DefaultWorkspaceID string

	// AI/Search rate limits — the ONLY per-tenant LLM control in this repository.
	//
	// Docs calls Lens with one service key for the whole instance and no balance/quota check
	// exists anywhere (see BUILD_STATE §0 Q3), so without these a single workspace can drive
	// unbounded Lens spend — and, if Lens meters per API key rather than per workspace label,
	// take AI down for every other tenant. These bound RATE, not cost.
	//
	// SIZING (measured, not guessed):
	//   AI completions are human-driven and expensive — a writer clicking "improve this"
	//   cannot meaningfully exceed 30/min. Default 30/min, burst 10.
	//
	//   Search is interactive and CHEAP per call, but the frontend debounces at 300ms and
	//   semantic search fires on the default type=all — so one person typing continuously
	//   drives ~200 embeddings/min. A completion-sized limit would break Cmd+K outright.
	//   Default 240/min, burst 40: generous for a few concurrent typists, still a ceiling.
	//
	// A non-positive value fails CLOSED (denies everything) rather than silently disabling
	// the limiter — see internal/ratelimit.New.
	AIRatePerMin     float64
	AIRateBurst      int
	SearchRatePerMin float64
	SearchRateBurst  int

	// Request-body caps (bytes). A memory bound, not a business rule: no route capped its
	// body before this, and PATCH /pages/{id} decodes into a map[string]any — a multi-GB
	// `content` was read into RAM, written to PG, walked by extractContentText and fanned to
	// the semantic indexer, with no container memory limit behind it.
	//
	// SIZING: MaxBodyBytes (4MB default) is far above any legitimate ProseMirror document —
	// a 4MB page is ~4M characters — while bounding the pathological case. The importer is
	// exempted to its own, much larger cap: it takes Confluence/Notion ZIP exports, and
	// internal/importer already declares 200MB as the largest reasonable space export.
	MaxBodyBytes       int64
	MaxImportBodyBytes int64

	// GatewayAuthSecret is SEC-4 Layer 1's root of trust: the shared secret the edge
	// gateway sends in x-gateway-auth to prove a request transited it (so the injected
	// x-user-email is trustworthy). It is the SAME secret the gateway signs for Track —
	// hence the unprefixed GATEWAY_AUTH_SECRET env, not a DOCS_-prefixed one. REQUIRED and
	// >= MinGatewayAuthSecretLen: without it every /v1 request is forgeable, so Docs
	// fail-closes at boot rather than run an open door.
	GatewayAuthSecret string
}

// MinGatewayAuthSecretLen mirrors Track — a short shared secret is brute-forceable, so a
// weak value is a boot failure, not a runtime surprise.
const MinGatewayAuthSecretLen = 16

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:            getEnv("DOCS_LISTEN_ADDR", "0.0.0.0:4000"),
		DatabaseURL:           os.Getenv("DOCS_DATABASE_URL"),
		LogLevel:              getEnv("DOCS_LOG_LEVEL", "info"),
		TrackURL:              os.Getenv("DOCS_TRACK_URL"),
		TrackAPIKey:           os.Getenv("DOCS_TRACK_API_KEY"),
		TrackMemberSyncSecret: os.Getenv("DOCS_TRACK_MEMBER_SYNC_SECRET"),
		LensURL:               os.Getenv("DOCS_LENS_URL"),
		LensAPIKey:            os.Getenv("DOCS_LENS_API_KEY"),
		DefaultWorkspaceID:    getEnv("DOCS_DEFAULT_WORKSPACE", "default"),
		GatewayAuthSecret:     os.Getenv("GATEWAY_AUTH_SECRET"),
		AIRatePerMin:          getEnvFloat("DOCS_AI_RATE_PER_MIN", 30),
		AIRateBurst:           getEnvInt("DOCS_AI_RATE_BURST", 10),
		SearchRatePerMin:      getEnvFloat("DOCS_SEARCH_RATE_PER_MIN", 240),
		SearchRateBurst:       getEnvInt("DOCS_SEARCH_RATE_BURST", 40),
		MaxBodyBytes:          int64(getEnvInt("DOCS_MAX_BODY_BYTES", 4<<20)),
		MaxImportBodyBytes:    int64(getEnvInt("DOCS_MAX_IMPORT_BODY_BYTES", 200<<20)),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("%w: DOCS_DATABASE_URL", ErrMissingEnv)
	}
	// SEC-4 Layer 1 root of trust — required and strong, or Docs won't boot (fail-closed:
	// an unset/weak gateway secret would leave every /v1 identity forgeable).
	if len(cfg.GatewayAuthSecret) < MinGatewayAuthSecretLen {
		return nil, fmt.Errorf("%w: GATEWAY_AUTH_SECRET must be set and >= %d chars", ErrMissingEnv, MinGatewayAuthSecretLen)
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvFloat reads a float env var, falling back when unset or unparseable. A malformed
// value takes the DEFAULT rather than 0 — 0 would fail the limiter closed and 429 every AI
// request, turning a typo into an outage.
func getEnvFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}

// getEnvInt is getEnvFloat for ints. Same malformed-value reasoning.
func getEnvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
