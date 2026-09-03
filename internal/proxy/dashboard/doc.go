// Package dashboard owns the read-only stats aggregation that the web UI
// renders. The proxy package wraps Aggregator results with cluster-state
// enrichment (drain progress, breaker health) before returning them to
// HTTP handlers.
package dashboard
