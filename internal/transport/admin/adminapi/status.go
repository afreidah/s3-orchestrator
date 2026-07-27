// -------------------------------------------------------------------------------
// Admin API - Shared Status DTOs
//
// Author: Alex Freidah
//
// Wire types for the admin status endpoint shared by the handler and its
// clients (adminctl, the TUI backends view). Kept in the leaf adminapi package
// so the server and out-of-process clients depend on one definition and the
// JSON shape cannot drift.
// -------------------------------------------------------------------------------

package adminapi

import "time"

// StatusResponse is a snapshot of instance and per-backend operational state.
type StatusResponse struct {
	DBHealthy   bool            `json:"db_healthy"`
	UsagePeriod string          `json:"usage_period"`
	Backends    []BackendStatus `json:"backends"`
}

// BackendStatus is the configured-and-live state of one backend: its quota and
// usage counters plus circuit-breaker health and drain state.
type BackendStatus struct {
	Name         string `json:"name"`
	Healthy      bool   `json:"healthy"`
	Draining     bool   `json:"draining"`
	BytesUsed    int64  `json:"bytes_used"`
	BytesLimit   int64  `json:"bytes_limit"`
	ObjectCount  int64  `json:"object_count"`
	APIRequests  int64  `json:"api_requests"`
	EgressBytes  int64  `json:"egress_bytes"`
	IngressBytes int64  `json:"ingress_bytes"`
}

// UsageReconcileResponse reports the per-backend byte corrections a reconcile
// pass applied. Adjustments maps a backend name to the delta written to its
// counter, negative when the counter was reduced; backends already in
// agreement with the object ledger are absent.
type UsageReconcileResponse struct {
	Status      string           `json:"status"`
	Adjustments map[string]int64 `json:"adjustments"`
}

// WorkersResponse is a snapshot of every registered background service's
// last-tick health.
type WorkersResponse struct {
	Workers []WorkerHealth `json:"workers"`
}

// WorkerHealth is one background service's last-tick outcome. Mirrors
// lifecycle.WorkerHealth, kept separate so the admin wire contract does not
// import the lifecycle package; the conversion between them is the single
// place field drift would surface.
type WorkerHealth struct {
	Name                string    `json:"name"`
	LastSuccess         time.Time `json:"last_success"`
	LastFailure         time.Time `json:"last_failure"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}
