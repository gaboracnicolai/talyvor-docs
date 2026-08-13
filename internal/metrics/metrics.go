// Package metrics exposes Prometheus counters + histograms.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	APIRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docs_api_requests_total",
			Help: "Total HTTP API requests by method, route, and status.",
		},
		[]string{"method", "route", "status"},
	)

	APILatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "docs_api_latency_seconds",
			Help:    "API latency by method and route, in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "route"},
	)

	// PagesCreated is deliberately UNLABELLED.
	//
	// It carried a `space_id` label, which broke both rules this file otherwise follows:
	//
	//  1. TENANT DISCLOSURE. /metrics is mounted outside the gatewayauth+authz boundary
	//     (cmd/docs/main.go), so anyone who can reach it could enumerate every space_id that
	//     had ever had a page created, plus each one's creation volume. Space ids are tenant
	//     identifiers; an unauthenticated endpoint must not emit them.
	//  2. UNBOUNDED CARDINALITY. metricsMiddleware bounds its labels to chi's RoutePattern()
	//     precisely so the series count cannot grow with traffic — and this label grew with
	//     every space ever created, contradicting that discipline in the same binary.
	//
	// The operational signal worth having is the RATE of page creation, which needs no label.
	//
	// ⚠ THE SENTENCE THAT USED TO FOLLOW WAS FALSE AND IS RETRACTED HERE RATHER THAN DELETED:
	// "Per-tenant creation counts belong behind the authenticated analytics API, which already
	// serves them (internal/analytics)". The first half is a judgement and stands. The second is
	// a claim about this repository and it is wrong: internal/analytics mounts exactly ONE route
	// (GET /v1/workspaces/{wsID}/analytics/pages) and it is READERSHIP — total_views,
	// unique_viewers, most/least read, never_read_count. There is no page-creation count in that
	// package, or in any other; the only COUNT(*) statements outside page_views are over
	// custom_domains and comments. So dropping the space_id label removed the per-tenant number
	// and pointed at a replacement that does not exist. Dropping it was still right (see 1 and 2
	// above) — what was not right was saying it had landed somewhere else.
	//
	// ⚠ INCREMENTED IN page.Store.Create, NOT IN A HANDLER, and that is load-bearing rather than
	// stylistic. The Inc lived in page.Handler.Create and was one of SIX doors into that single
	// INSERT, so `create_page` (MCP), template instantiation and every bulk import moved this
	// number by zero. Pinned per surface by pagescreated_metric_realpg_test.go in internal/page,
	// internal/mcp, internal/templatelib and internal/importer.
	PagesCreated = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "docs_pages_created_total",
			Help: "Pages created, by any route (REST, MCP, template, import); deliberately unlabelled — /metrics is unauthenticated.",
		},
	)
)

func init() {
	prometheus.MustRegister(APIRequests, APILatency, PagesCreated)
}

func Handler() http.Handler { return promhttp.Handler() }
