// Package openapi turns a list of route descriptors into an OpenAPI 3.1
// document. Callers hand over what their route table already knows - method,
// path, summary, parameters and exchanged types - and every schema is reflected
// from those types rather than written by hand.
//
// Output is deterministic, so a regenerated document is byte-identical when
// nothing changed, which is what lets a test diff it against the committed
// copy.
//
// Generator-only: importing it from server code would link a documentation tool
// into the daemon, so it is imported from tests alone.
package openapi
