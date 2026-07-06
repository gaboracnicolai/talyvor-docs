// Package config loads server settings from environment variables.
// Empty values are tolerated for non-critical fields; the only hard
// requirement is DOCS_DATABASE_URL, without which the server can't
// reach Postgres and exits at boot.
package config

import (
	"errors"
	"fmt"
	"os"
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
