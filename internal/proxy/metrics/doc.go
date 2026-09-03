// Package metrics owns the Prometheus gauge/counter recording for the proxy
// package. It exposes a Collector that the orchestrator embeds to refresh
// gauge values from the metadata store and record per-operation metrics.
package metrics
