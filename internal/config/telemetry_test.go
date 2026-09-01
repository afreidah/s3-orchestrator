// -------------------------------------------------------------------------------
// Telemetry Config Tests
//
// Author: Alex Freidah
//
// The metrics listener's failure policy. The default decides whether a bind
// conflict is a startup error or a warning nobody reads, so it is worth pinning
// rather than inferring from the struct.
// -------------------------------------------------------------------------------

package config

import "testing"

// TestMetricsConfig_ListenerRequiredDefault pins the default an omitted field
// takes: required, because a production deployment expects metrics and the
// alternative is serving traffic while Prometheus silently receives nothing.
func TestMetricsConfig_ListenerRequiredDefault(t *testing.T) {
	t.Parallel()
	if !(MetricsConfig{}).ListenerRequired() {
		t.Error("an omitted require_listener must default to required")
	}

	on, off := true, false
	if !(MetricsConfig{RequireListener: &on}).ListenerRequired() {
		t.Error("an explicit true must be honoured")
	}
	if (MetricsConfig{RequireListener: &off}).ListenerRequired() {
		t.Error("an explicit false must be honoured; it is the dev opt-out")
	}
}
