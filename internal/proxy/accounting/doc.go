// Package accounting centralizes the per-backend usage rules every storage
// subsystem follows: one API-call charge per backend call regardless of
// outcome, egress bytes on top for a successful read, ingress bytes for a
// successful write, and the operation/backend/start/error tuple the Prometheus
// histogram records.
//
// Keeping the rules here rather than at each call site is what stops a new
// caller from charging bandwidth twice or from billing a backend it never
// reached.
package accounting
