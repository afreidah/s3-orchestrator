// -------------------------------------------------------------------------------
// Metrics - Prometheus Instrumentation Index
//
// Author: Alex Freidah
//
// Subsystem-scoped Prometheus surface for the S3 orchestrator. The metric
// definitions are split across metrics_<domain>.go files so each file stays
// under ~150 lines and one subsystem's metrics live alongside each other.
// All metrics are prefixed with "s3o_" and registered via promauto on init
// through the constructors below.
// -------------------------------------------------------------------------------

package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// counter registers a counter with the default registry.
func counter(name, help string) prometheus.Counter {
	return promauto.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
}

// counterVec registers a counter family partitioned by labels with the
// default registry.
func counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	return promauto.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
}

// gauge registers a gauge with the default registry.
func gauge(name, help string) prometheus.Gauge {
	return promauto.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
}

// gaugeVec registers a gauge family partitioned by labels with the default
// registry.
func gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	return promauto.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
}

// histogram registers a histogram over buckets with the default registry.
func histogram(name, help string, buckets []float64) prometheus.Histogram {
	return promauto.NewHistogram(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets})
}

// histogramVec registers a histogram family over buckets, partitioned by
// labels, with the default registry.
func histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	return promauto.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
}
