// -------------------------------------------------------------------------------
// Metrics - Prometheus Instrumentation
//
// Project: Munchbox / Author: Alex Freidah
//
// Prometheus metric definitions for the S3 proxy. Tracks request counts, latencies,
// sizes, and backend health. All metrics are prefixed with 's3proxy_' for easy
// identification in dashboards and alerting rules.
// -------------------------------------------------------------------------------

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// -------------------------------------------------------------------------
// METRIC DEFINITIONS
// -------------------------------------------------------------------------

var (
	// --- Request metrics ---

	// RequestsTotal counts all HTTP requests by method and status code.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_requests_total",
			Help: "Total number of HTTP requests processed",
		},
		[]string{"method", "status_code"},
	)

	// RequestDuration tracks request latency distribution by method.
	RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_request_duration_seconds",
			Help:    "HTTP request latency in seconds",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"method"},
	)

	// RequestSize tracks upload sizes.
	RequestSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_request_size_bytes",
			Help:    "HTTP request body size in bytes",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 10), // 1KB to 256GB
		},
		[]string{"method"},
	)

	// ResponseSize tracks download sizes.
	ResponseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_response_size_bytes",
			Help:    "HTTP response body size in bytes",
			Buckets: prometheus.ExponentialBuckets(1024, 4, 10), // 1KB to 256GB
		},
		[]string{"method"},
	)

	// InflightRequests tracks currently processing requests.
	InflightRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3proxy_inflight_requests",
			Help: "Number of requests currently being processed",
		},
		[]string{"method"},
	)

	// --- Backend metrics ---

	// BackendRequestsTotal counts backend operations by operation type and status.
	BackendRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_backend_requests_total",
			Help: "Total number of backend storage operations",
		},
		[]string{"operation", "backend", "status"},
	)

	// BackendDuration tracks backend operation latency.
	BackendDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_backend_duration_seconds",
			Help:    "Backend operation latency in seconds",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120},
		},
		[]string{"operation", "backend"},
	)

	// --- Info metric ---

	// BuildInfo exposes version information.
	BuildInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3proxy_build_info",
			Help: "Build information for the S3 proxy",
		},
		[]string{"version", "go_version"},
	)
)
