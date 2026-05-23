// -------------------------------------------------------------------------------
// HTTP Server - Health and Readiness Endpoints
//
// Author: Alex Freidah
//
// /health reports overall service status (degraded when the database circuit
// breaker is open) and is always available. /health/ready returns 503 until
// the orchestrator has finished resolving services and registering handlers,
// so load balancers do not route to a half-started instance.
// -------------------------------------------------------------------------------

package httpserver

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
)

// HealthDeps holds the signals the health and readiness handlers read.
type HealthDeps struct {
	// Ready reports whether the daemon has finished initialization and is
	// accepting traffic. Routed to /health/ready.
	Ready *atomic.Bool
	// DBBreaker resolves the database circuit breaker lazily so the health
	// handler reflects breaker state without holding a reference past
	// shutdown. nil is treated as healthy.
	DBBreaker func() *breaker.CircuitBreaker
}

// registerHealthEndpoints mounts /health and /health/ready on mux.
func registerHealthEndpoints(mux *http.ServeMux, deps HealthDeps) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		status := "ok"
		if cb := deps.DBBreaker(); cb != nil && !cb.IsHealthy() {
			status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !deps.Ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"not ready"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	})
}
