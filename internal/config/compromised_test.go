package config_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/config"
)

// publishedSecret is the value docker-compose.yaml and .env.example both shipped as the
// GATEWAY_AUTH_SECRET default. It is 42 characters, so it satisfied the >= 16 length check
// while being readable by anyone with the repo — and GATEWAY_AUTH_SECRET is the whole root
// of trust: knowing it means forging x-gateway-auth + x-user-email and being any user in
// any workspace.
//
// It is in git history, so deleting it from HEAD does not un-publish it. Treat as
// permanently compromised.
const publishedSecret = "dev-only-insecure-gateway-secret-change-me"

// THE LENGTH CHECK IS NOT THE GUARD. A long placeholder passes it. The guard has to reject
// the specific values that have been published, independently of length.
//
// RED (pre-fix): Load() ACCEPTS publishedSecret because len(42) >= 16.
func TestLoad_RejectsPublishedSecret(t *testing.T) {
	t.Setenv("DOCS_DATABASE_URL", "postgres://x")
	t.Setenv("GATEWAY_AUTH_SECRET", publishedSecret)

	if len(publishedSecret) < config.MinGatewayAuthSecretLen {
		t.Fatalf("premise wrong: the published secret is %d chars, under the %d minimum — "+
			"the length check would already have caught it",
			len(publishedSecret), config.MinGatewayAuthSecretLen)
	}
	_, err := config.Load()
	if err == nil {
		t.Fatalf("Load() ACCEPTED the published secret (%d chars). A guard a shipped default "+
			"satisfies is not a guard: any reader of this repo can forge every identity.",
			len(publishedSecret))
	}
	if !errors.Is(err, config.ErrMissingEnv) {
		t.Errorf("rejection should wrap ErrMissingEnv for consistent boot handling, got %v", err)
	}
	// The operator has to be able to tell this apart from "you forgot to set it".
	if !strings.Contains(strings.ToLower(err.Error()), "publish") &&
		!strings.Contains(strings.ToLower(err.Error()), "compromis") {
		t.Errorf("error should say the value is published/compromised, not merely missing: %v", err)
	}
}

// Rejection must be surgical: an operator-generated secret of the same length still loads,
// and the existing unset/short behaviour is unchanged.
func TestLoad_StillAcceptsRealSecretsAndRejectsWeakOnes(t *testing.T) {
	t.Setenv("DOCS_DATABASE_URL", "postgres://x")

	// Same length as the published one, but not it.
	t.Setenv("GATEWAY_AUTH_SECRET", "a3f9c1e07b52d84af61c9e3b0d7a5182c4e6f9b0d1")
	if _, err := config.Load(); err != nil {
		t.Errorf("a real 42-char secret must still load: %v", err)
	}
	t.Setenv("GATEWAY_AUTH_SECRET", "")
	if _, err := config.Load(); !errors.Is(err, config.ErrMissingEnv) {
		t.Errorf("unset must still fail, got %v", err)
	}
	t.Setenv("GATEWAY_AUTH_SECRET", "tooshort")
	if _, err := config.Load(); !errors.Is(err, config.ErrMissingEnv) {
		t.Errorf("short must still fail, got %v", err)
	}
}

// The config-side rejection is only half the fix. Compose must not SUPPLY a value at all:
// `${VAR:-default}` silently substitutes, so `docker compose up` with no operator input
// produced a running, fully forgeable server. It has to fail instead.
//
// RED (pre-fix): docker-compose.yaml:24 carries `${GATEWAY_AUTH_SECRET:-dev-only-...}`.
func TestComposeDoesNotSupplyAGatewaySecret(t *testing.T) {
	b, err := os.ReadFile("../../docker-compose.yaml")
	if err != nil {
		t.Fatalf("read compose: %v", err)
	}
	compose := string(b)

	if strings.Contains(compose, publishedSecret) {
		t.Errorf("docker-compose.yaml still ships the published secret verbatim")
	}
	// A `:-` default on this variable defeats the fail-closed boot check. Scan only ACTIVE
	// lines: the comment there documents the removed defect (and quotes its shape), which is
	// worth keeping — matching inside comments would forbid explaining the fix.
	fallback := regexp.MustCompile(`\$\{GATEWAY_AUTH_SECRET:-[^}]*\}`)
	for i, line := range strings.Split(compose, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := fallback.FindString(line); m != "" {
			t.Errorf("compose:%d supplies a fallback for the root of trust: %s\n"+
				"`:-` means `docker compose up` starts a forgeable server with no operator input; "+
				"the variable must be required so startup fails loudly instead", i+1, m)
		}
	}
	// And it must be actively REQUIRED, not merely defaulted-away — `:?` makes compose itself
	// refuse with a named error before the container is created.
	if !regexp.MustCompile(`(?m)^\s*-\s*GATEWAY_AUTH_SECRET=\$\{GATEWAY_AUTH_SECRET:\?`).MatchString(compose) {
		t.Error("compose must declare GATEWAY_AUTH_SECRET with `:?` so startup fails when it is unset")
	}
}

// .env.example is copied to .env verbatim by every quick-start. Shipping a WORKING value
// there is the same defect as the compose default: it produces a bootable, forgeable
// deployment. The template must carry the instruction, not a usable secret.
func TestEnvExampleShipsNoUsableSecret(t *testing.T) {
	b, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	env := string(b)

	if strings.Contains(env, publishedSecret) {
		t.Errorf(".env.example still ships the published secret; copying it to .env yields a " +
			"bootable, fully forgeable deployment")
	}
	// Find the assignment and confirm it is empty (or absent).
	for _, line := range strings.Split(env, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "GATEWAY_AUTH_SECRET=") {
			continue
		}
		if v := strings.TrimSpace(strings.TrimPrefix(line, "GATEWAY_AUTH_SECRET=")); v != "" {
			t.Errorf("GATEWAY_AUTH_SECRET in .env.example must be empty so a copied template "+
				"fails the boot check; got a value (%d chars)", len(v))
		}
	}
}

// DOCS_LOG_LEVEL was parsed into Config and never applied — the logger was built with nil
// HandlerOptions, pinning the level at Info — so `debug` was documented, accepted and inert.
// Asserting on the absence of DEBUG lines at boot would prove nothing (nothing logs at DEBUG
// during startup), so test the mapping itself, plus that a handler built from it admits a
// Debug record.
func TestSlogLevel_HonoursDocsLogLevel(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" warn ", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"nonsense", slog.LevelInfo}, // a typo must not take the service down
	} {
		t.Setenv("DOCS_DATABASE_URL", "postgres://x")
		t.Setenv("GATEWAY_AUTH_SECRET", "a3f9c1e07b52d84af61c9e3b0d7a5182c4e6f9b0d1")
		t.Setenv("DOCS_LOG_LEVEL", tc.env)
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("load(%q): %v", tc.env, err)
		}
		if got := cfg.SlogLevel(); got != tc.want {
			t.Errorf("DOCS_LOG_LEVEL=%q → %v, want %v", tc.env, got, tc.want)
		}
	}

	// End to end: a handler built the way main.go builds it must actually EMIT a Debug record
	// when the level is debug — the property that was missing.
	t.Setenv("DOCS_LOG_LEVEL", "debug")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: cfg.SlogLevel()})).
		Debug("visible-at-debug")
	if !strings.Contains(buf.String(), "visible-at-debug") {
		t.Error("DOCS_LOG_LEVEL=debug must admit Debug records; got none")
	}
	// And the old behaviour (nil options) must NOT — proving the two differ.
	buf.Reset()
	slog.New(slog.NewJSONHandler(&buf, nil)).Debug("visible-at-debug")
	if buf.Len() != 0 {
		t.Error("control failed: nil HandlerOptions should suppress Debug")
	}
}
