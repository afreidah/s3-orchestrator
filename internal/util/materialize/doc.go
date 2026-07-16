// Package materialize turns a one-shot stream into a seekable, re-readable
// payload without scaling heap with object size: bytes below MemThreshold stay
// in memory, larger payloads spill to a self-unlinking tempfile. Body.Reader
// hands back a fresh io.ReadSeeker positioned at offset 0 on every call, so
// callers that must replay a body - PUT failover, encryption layers, and the
// AWS SDK's SigV4 payload-hash pass for signed uploads - can consume it
// repeatedly. The sink is engine- and transport-neutral so both the backend
// adapters and the proxy write path share one implementation.
package materialize
