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
	// Per-tenant creation counts belong behind the authenticated analytics API, which already
	// serves them (internal/analytics), not in an open metrics scrape.
	PagesCreated = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "docs_pages_created_total",
			Help: "Pages created (all spaces; deliberately unlabelled — /metrics is unauthenticated).",
		},
	)
)

func init() {
	prometheus.MustRegister(APIRequests, APILatency, PagesCreated)
}

func Handler() http.Handler { return promhttp.Handler() }
