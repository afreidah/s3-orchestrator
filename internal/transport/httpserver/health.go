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
	"net/http"
	"sync/atomic"

	"github.com/afreidah/s3-orchestrator/internal/breaker"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// HealthDeps holds the signals the health and readiness handlers read. Ready
// reports initialization finished and traffic is accepted, served at
// /health/ready. DBBreaker is a function rather than a reference so the
// handler reflects breaker state without holding one past shutdown; nil reads
// as healthy.
type HealthDeps struct {
	Ready     *atomic.Bool
	DBBreaker func() *breaker.CircuitBreaker
}

// healthStatus is the body both endpoints answer with.
type healthStatus struct {
	Status string `json:"status"`
}

// registerHealthEndpoints mounts /health and /health/ready on mux.
func registerHealthEndpoints(mux *http.ServeMux, deps HealthDeps) {
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		status := "ok"
		if cb := deps.DBBreaker(); cb != nil && !cb.IsHealthy() {
			status = "degraded"
		}
		httputil.WriteJSON(w, http.StatusOK, healthStatus{Status: status})
	})

	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		if !deps.Ready.Load() {
			httputil.WriteJSON(w, http.StatusServiceUnavailable, healthStatus{Status: "not ready"})
			return
		}
		httputil.WriteJSON(w, http.StatusOK, healthStatus{Status: "ready"})
	})
}
