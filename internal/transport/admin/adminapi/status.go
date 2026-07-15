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
