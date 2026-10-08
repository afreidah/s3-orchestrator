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
	Integrity   IntegrityStatus `json:"integrity"`
}

// IntegrityStatus reports how far behind content verification is. Distinct
// from a copy lacking a hash: a fleet can be fully hashed and unverified.
//
// OldestUnverifiedSeconds and NeverVerifiedCopies cover only copies the sweep
// can reach. DeferredCopies are on backends over their usage limit and stay
// unverified until that clears. PlaintextCopies stays non-zero until
// encrypt-existing is run, since encryption covers new writes only.
type IntegrityStatus struct {
	OldestUnverifiedSeconds int64 `json:"oldest_unverified_seconds"`
	NeverVerifiedCopies     int64 `json:"never_verified_copies"`
	DeferredCopies          int64 `json:"deferred_copies"`
	PlaintextCopies         int64 `json:"plaintext_copies"`
}

// BackendStatus is the configured-and-live state of one backend: its quota and
// usage counters plus circuit-breaker health and drain state.
//
// Draining is true only while a drain is in progress. DrainState is the drain
// record's state (draining, drained, or failed) and empty when the backend has
// none; a backend with any state is refused new writes.
//
// CompressionSavedBytes is the logical size of this backend's compressed
// objects less what they occupy; zero when nothing there is stored encoded.
type BackendStatus struct {
	Name         string `json:"name"`
	Healthy      bool   `json:"healthy"`
	Draining     bool   `json:"draining"`
	DrainState   string `json:"drain_state,omitempty"`
	BytesUsed    int64  `json:"bytes_used"`
	BytesLimit   int64  `json:"bytes_limit"`
	ObjectCount  int64  `json:"object_count"`
	APIRequests  int64  `json:"api_requests"`
	EgressBytes  int64  `json:"egress_bytes"`
	IngressBytes int64  `json:"ingress_bytes"`

	CompressionSavedBytes int64 `json:"compression_saved_bytes"`
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

// UsageFlushResponse acknowledges a forced flush of the usage counters.
type UsageFlushResponse struct {
	Status string `json:"status"`
}

// LogLevelResponse is the runtime log level, returned by both the read and the
// write form of the endpoint so a caller sees the level actually in effect.
type LogLevelResponse struct {
	Level string `json:"level"`
}

// ReloadStatusResponse reports the most recent config reload. Status is
// "no_reload_yet" until SIGHUP fires for the first time, and every other field
// is absent in that state; afterwards it carries the coordinator's outcome for
// the pass. Generation is a pointer so a real generation 0 is still reported.
type ReloadStatusResponse struct {
	Status          string              `json:"status"`
	Generation      *int64              `json:"generation,omitempty"`
	Outcomes        []ReloadHookOutcome `json:"outcomes,omitempty"`
	RequiresRestart []string            `json:"requires_restart,omitempty"`
	LoadError       string              `json:"load_error,omitempty"`
	StartedAt       *time.Time          `json:"started_at,omitempty"`
	EndedAt         *time.Time          `json:"ended_at,omitempty"`
}

// ConfigResponse is the running configuration as YAML with secrets redacted, the
// log level in effect, and file changes since startup that wait on a restart.
type ConfigResponse struct {
	Config         string   `json:"config"`
	LogLevel       string   `json:"log_level"`
	PendingRestart []string `json:"pending_restart,omitempty"`
}

// ReloadHookOutcome is one subsystem's contribution to a reload pass. Skipped
// hooks are reported too, so an operator can see which subsystems the reload
// did not touch.
type ReloadHookOutcome struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SetLogLevelRequest is the body of the log-level write: the level to apply.
// Valid values are debug, info, warn and error.
type SetLogLevelRequest struct {
	Level string `json:"level"`
}
