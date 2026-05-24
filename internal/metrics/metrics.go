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

	PagesCreated = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docs_pages_created_total",
			Help: "Pages created by space.",
		},
		[]string{"space_id"},
	)
)

func init() {
	prometheus.MustRegister(APIRequests, APILatency, PagesCreated)
}

func Handler() http.Handler { return promhttp.Handler() }
