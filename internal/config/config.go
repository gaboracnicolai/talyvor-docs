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

	// DefaultWorkspaceID is the tenant the cost syncer operates
	// against. Phase 4 supports a single workspace per Docs
	// instance; Phase 5 will iterate workspaces from the DB.
	DefaultWorkspaceID string
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:         getEnv("DOCS_LISTEN_ADDR", "0.0.0.0:4000"),
		DatabaseURL:        os.Getenv("DOCS_DATABASE_URL"),
		LogLevel:           getEnv("DOCS_LOG_LEVEL", "info"),
		TrackURL:           os.Getenv("DOCS_TRACK_URL"),
		TrackAPIKey:        os.Getenv("DOCS_TRACK_API_KEY"),
		LensURL:            os.Getenv("DOCS_LENS_URL"),
		DefaultWorkspaceID: getEnv("DOCS_DEFAULT_WORKSPACE", "default"),
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("%w: DOCS_DATABASE_URL", ErrMissingEnv)
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
