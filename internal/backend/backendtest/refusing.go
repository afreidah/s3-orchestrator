// -------------------------------------------------------------------------------
// Backendtest - Refusing Backend
//
// Author: Alex Freidah
//
// A circuit-breaker-wrapped backend with one call's breaker already open, so a
// test can put a partly degraded backend in a fleet without driving failures
// through it first. It looks the way a provider over its monthly download
// quota does: GetObject refused, every other call served.
// -------------------------------------------------------------------------------

package backendtest

import (
	"context"
	"net/http"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
)

// RefusingReads returns an in-memory backend named name whose GET breaker is
// open and stays open for the test, while every other call is served.
func RefusingReads(name string) *backend.CircuitBreakerBackend {
	inner := NewInMemory()
	inner.GetErr = &StatusError{Code: http.StatusForbidden, Msg: "download quota exceeded"}
	cb := backend.NewCircuitBreakerBackend(inner, backend.CircuitBreakerConfig{Name: name, Threshold: 1, Timeout: time.Hour})
	_, _ = cb.GetObject(context.Background(), "refused", "")
	return cb
}
