package config_test

import (
	"errors"
	"testing"

	"github.com/talyvor/docs/internal/config"
)

// SEC-4 Layer 1 fail-closed: GATEWAY_AUTH_SECRET is the root of trust, so an unset or weak
// value must abort boot — never run with a forgeable /v1 boundary.
func TestLoad_GatewayAuthSecret_BootFailsClosed(t *testing.T) {
	t.Setenv("DOCS_DATABASE_URL", "postgres://x")

	t.Setenv("GATEWAY_AUTH_SECRET", "") // unset → fail
	if _, err := config.Load(); !errors.Is(err, config.ErrMissingEnv) {
		t.Fatalf("unset GATEWAY_AUTH_SECRET must fail Load, got %v", err)
	}

	t.Setenv("GATEWAY_AUTH_SECRET", "tooshort") // < 16 → fail
	if _, err := config.Load(); !errors.Is(err, config.ErrMissingEnv) {
		t.Fatalf("short GATEWAY_AUTH_SECRET must fail Load, got %v", err)
	}

	t.Setenv("GATEWAY_AUTH_SECRET", "a-strong-gateway-secret-0123456789") // >= 16 → ok
	if _, err := config.Load(); err != nil {
		t.Fatalf("valid GATEWAY_AUTH_SECRET must load: %v", err)
	}
}
