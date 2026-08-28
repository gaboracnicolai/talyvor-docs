package config_test

import (
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/talyvor/docs/internal/config"
)

// THE SHIPPED DEFAULTS ARE NOW PINNED. Until this file existed, they were not.
//
// MEASURED 2026-08-28 (tab-k2w8, W3.29) by mutation, not by reading:
// ~/talyvor-queue/w329-unpinned-defaults-census-k2w8.py flips one literal
// default at a time in the safe->unsafe direction, compiles it, and runs the
// WHOLE suite against a real Postgres. Population 14 (every literal default in
// Load(), plus the two SEC-4 boot constants). Result: 12 UNPINNED, 2 CAUGHT.
//
// The two CAUGHT are MinGatewayAuthSecretLen and publishedGatewaySecrets — the
// two values that gate IDENTITY, both already defended by config_test.go and
// compromised_test.go. EVERY OTHER VALUE, including every ceiling that gates
// AI and embedding SPEND, could be set to effectively infinite and all 40
// packages stayed green:
//
//	AIRatePerMin 30 -> 1e9 · SearchRatePerMin 240 -> 1e9 · IndexRatePerMin
//	300 -> 1e9 · all three bursts -> 1e9 · MaxBodyBytes 4MB -> 1TB ·
//	MaxImportBodyBytes 200MB -> 1TB · ListenAddr · DefaultWorkspaceID ->
//	"" (config.go calls that default a trap) · IndexStalenessSec · IndexWorkers
//
// ⚠ THIS FILE CHANGES NO VALUE. It RECORDS the ones that ship, so that changing
// one becomes a deliberate edit to a named table instead of a silent one-token
// diff. Every number here is the number that was already in config.go; the
// question of whether any of them is the RIGHT number is a product decision and
// is deliberately not taken here.

// shippedDefaults is the recorded population. Keyed by env var so the
// completeness floor below can compare it against config.go itself.
var shippedDefaults = map[string]struct {
	desc string
	get  func(*config.Config) any
	want any
}{
	"DOCS_LISTEN_ADDR":           {"bind address", func(c *config.Config) any { return c.ListenAddr }, "0.0.0.0:4000"},
	"DOCS_LOG_LEVEL":             {"log level", func(c *config.Config) any { return c.LogLevel }, "info"},
	"DOCS_DEFAULT_WORKSPACE":     {"last-resort workspace fallback", func(c *config.Config) any { return c.DefaultWorkspaceID }, "default"},
	"DOCS_AI_RATE_PER_MIN":       {"AI spend ceiling", func(c *config.Config) any { return c.AIRatePerMin }, 30.0},
	"DOCS_AI_RATE_BURST":         {"AI burst", func(c *config.Config) any { return c.AIRateBurst }, 10},
	"DOCS_SEARCH_RATE_PER_MIN":   {"search ceiling (gates embed spend)", func(c *config.Config) any { return c.SearchRatePerMin }, 240.0},
	"DOCS_SEARCH_RATE_BURST":     {"search burst", func(c *config.Config) any { return c.SearchRateBurst }, 40},
	"DOCS_INDEX_WORKERS":         {"index worker count", func(c *config.Config) any { return c.IndexWorkers }, 4},
	"DOCS_INDEX_RATE_PER_MIN":    {"index ceiling (gates embed spend)", func(c *config.Config) any { return c.IndexRatePerMin }, 300.0},
	"DOCS_INDEX_RATE_BURST":      {"index burst", func(c *config.Config) any { return c.IndexRateBurst }, 10},
	"DOCS_INDEX_STALENESS_SEC":   {"re-embed staleness window", func(c *config.Config) any { return c.IndexStalenessSec }, 5},
	"DOCS_MAX_BODY_BYTES":        {"request body ceiling", func(c *config.Config) any { return c.MaxBodyBytes }, int64(4 << 20)},
	"DOCS_MAX_IMPORT_BODY_BYTES": {"import body ceiling", func(c *config.Config) any { return c.MaxImportBodyBytes }, int64(200 << 20)},
}

// loadWithDefaults runs Load() with every defaulted key blanked, so what comes
// back IS the shipped default and not the developer's environment. The two
// required vars are set to valid values; a blank GATEWAY_AUTH_SECRET would fail
// boot and this test would be measuring the SEC-4 path instead.
func loadWithDefaults(t *testing.T) *config.Config {
	t.Helper()
	for key := range shippedDefaults {
		t.Setenv(key, "") // empty == unset for getEnv/getEnvInt/getEnvFloat
	}
	t.Setenv("DOCS_DATABASE_URL", "postgres://x")
	t.Setenv("GATEWAY_AUTH_SECRET", "a-strong-gateway-secret-0123456789")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() with a blank environment must succeed: %v", err)
	}
	return cfg
}

func TestLoad_ShippedDefaults(t *testing.T) {
	cfg := loadWithDefaults(t)
	for key, d := range shippedDefaults {
		if got := d.get(cfg); got != d.want {
			t.Errorf("%s (%s): Load() defaulted to %v, recorded default is %v.\n"+
				"If this change is deliberate, change the recorded value here in the same "+
				"commit and say why — that is the entire point of this file.", key, d.desc, got, d.want)
		}
	}
}

// TestLoad_EveryDefaultIsRecorded is the completeness floor, and it is why this
// guard cannot quietly narrow. The population is DERIVED by parsing config.go
// rather than restated, so a new getEnv* default that nobody adds to the table
// reds this test instead of silently escaping the census above.
func TestLoad_EveryDefaultIsRecorded(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("cannot read config.go — the population this guard measures is gone: %v", err)
	}
	re := regexp.MustCompile(`getEnv(?:Int|Float)?\("([A-Z_]+)"`)
	m := re.FindAllStringSubmatch(string(src), -1)
	if len(m) == 0 {
		t.Fatal("parsed ZERO getEnv* call sites out of config.go — the parser is broken, " +
			"and a guard that finds nothing to check is the defect it exists to catch")
	}
	found := map[string]bool{}
	for _, g := range m {
		found[g[1]] = true
	}
	var missing, extra []string
	for key := range found {
		if _, ok := shippedDefaults[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range shippedDefaults {
		if !found[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("config.go defaults NOT recorded in shippedDefaults: %v.\n"+
			"Add them — an unrecorded default is one nothing in this repository defends.", missing)
	}
	if len(extra) > 0 {
		t.Errorf("shippedDefaults records keys config.go no longer defaults: %v.\n"+
			"Remove them — a guard pinning a value that no longer exists is inert.", extra)
	}
}
