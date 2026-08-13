package testutil

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/talyvor/docs/internal/metrics"
)

// ScrapeCounter returns the current value of a plain (unlabelled) counter AS AN OPERATOR
// WOULD READ IT — by driving the SHIPPED /metrics handler (metrics.Handler(), the exact
// http.Handler cmd/docs/main.go mounts) and parsing the exposition text it answers with.
//
// ⚠ IT SCRAPES RATHER THAN READING THE COUNTER OBJECT, and that is the point. A test that
// reads `metrics.PagesCreated` through the client library asserts that a variable moved; the
// question a page-creation metric exists to answer is whether the number an operator SEES
// moved. The two differ whenever a counter is not registered, is registered on a registry the
// handler does not gather, or is renamed — and every one of those is invisible to a direct
// read of the variable.
//
// The name must be present. A registered unlabelled counter is exported even at zero, so
// absence is a real failure (unregistered, renamed, or turned into a *Vec with no series) and
// is reported as one rather than defaulted to 0 — a silent 0 would make every delta assertion
// in this repository pass against a metric that had stopped existing.
func ScrapeCounter(t *testing.T, name string) float64 {
	t.Helper()
	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("testutil: /metrics answered HTTP %d, want 200", rr.Code)
	}
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok || key != name {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("testutil: %s exported as %q, which is not a number: %v", name, value, err)
		}
		return v
	}
	t.Fatalf("testutil: %s is absent from the /metrics scrape — a registered counter is exported "+
		"even at zero, so this means it is unregistered, renamed, or no longer a plain counter", name)
	return 0 // unreachable; t.Fatalf stops the test
}
