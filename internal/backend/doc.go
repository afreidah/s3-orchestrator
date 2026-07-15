// Package backend defines the ObjectBackend abstraction over the S3-compatible
// providers the orchestrator stores objects on, plus the wrappers that harden
// it. ObjectBackend is the narrow streaming interface (GET/PUT/DELETE/list and
// multipart) the read and write paths depend on; concrete implementations
// adapt a single provider bucket.
//
// CircuitBreakerBackend wraps an ObjectBackend so a provider with expired
// credentials or an outage is automatically excluded from routing after
// consecutive failures, with a single probe request testing recovery once the
// open timeout elapses. The package also centralizes not-found classification
// so callers can distinguish "the object is genuinely absent" from a transport
// error without matching provider-specific error shapes.
package backend
