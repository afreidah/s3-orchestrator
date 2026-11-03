// Package backend defines the ObjectBackend abstraction over the S3-compatible
// providers the orchestrator stores objects on, plus the wrappers that harden
// it. ObjectBackend is the narrow streaming interface (GET/PUT/DELETE/list and
// multipart) the read and write paths depend on; concrete implementations
// adapt a single provider bucket.
//
// CircuitBreakerBackend wraps an ObjectBackend so a failing provider, or one
// refusing a single call, stops being called until an out-of-band check shows
// it has recovered. The package also centralizes not-found classification so
// callers can tell an absent object from a transport error without matching
// provider-specific error shapes.
package backend
