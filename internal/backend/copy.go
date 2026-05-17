// -------------------------------------------------------------------------------
// Backend Copy Capability
//
// Author: Alex Freidah
//
// Optional capability interface a backend can implement to expose a
// server-side (native) CopyObject operation. The orchestrator type-asserts
// to BackendCopier to detect support, so backends that lack native copy
// are unaffected and the proxy falls back to materialized copy (GET on the
// source, buffer/spill, PUT on the destination).
//
// Native copy is only meaningful when both keys live on the same backend.
// Cross-backend copies still flow through the orchestrator because no
// single backend can satisfy a CopySource from a foreign bucket.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
)

// BackendCopier is the optional capability for backends that support a
// server-side CopyObject. Backends that implement this method enable the
// orchestrator's same-backend fast path: instead of GET-then-PUT through
// the proxy, a single CopyObject call asks the backend to copy bytes
// internally, eliminating proxy CPU, memory, and network cost.
//
// Callers should check ok via a type assertion and fall back to
// materialized copy when the backend does not implement this interface
// or when it returns ErrCopyNotSupported (e.g., a decorator forwarding
// to an underlying backend that lacks native copy).
type BackendCopier interface {
	CopyObject(ctx context.Context, srcKey, dstKey, contentType string, metadata map[string]string) (etag string, err error)
}

// ErrCopyNotSupported signals that a backend (or a decorator wrapping
// one) cannot perform a server-side copy. Callers should fall back to
// materialized copy. Returned by decorators like CircuitBreakerBackend
// when their wrapped backend does not implement BackendCopier.
var ErrCopyNotSupported = errors.New("backend does not support native copy")
