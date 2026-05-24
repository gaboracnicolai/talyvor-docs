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
	TrackURL string
	LensURL  string
}

func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr:  getEnv("DOCS_LISTEN_ADDR", "0.0.0.0:4000"),
		DatabaseURL: os.Getenv("DOCS_DATABASE_URL"),
		LogLevel:    getEnv("DOCS_LOG_LEVEL", "info"),
		TrackURL:    os.Getenv("DOCS_TRACK_URL"),
		LensURL:     os.Getenv("DOCS_LENS_URL"),
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
