// -------------------------------------------------------------------------------
// Telemetry Configuration
//
// Author: Alex Freidah
//
// Defines TelemetryConfig: the OpenTelemetry exporter endpoint, sample
// rate, service name and version, plus the optional Prometheus exporter
// listen address. Carried into telemetry.Init at startup so the metrics
// surface and tracing pipeline are wired before any background worker
// runs. Empty fields disable that exporter rather than failing startup.
// -------------------------------------------------------------------------------

package config

import "cmp"

// TelemetryConfig holds observability settings.
type TelemetryConfig struct {
	Metrics MetricsConfig `yaml:"metrics"`
	Tracing TracingConfig `yaml:"tracing"`
}

// MetricsConfig holds Prometheus metrics settings.
//
// Pprof is opt-in and off by default. The net/http/pprof handlers
// expose deep runtime state (de-anonymized stack frames, command-line
// flags, on-demand CPU profiles that double as DoS amplifiers), so
// production deployments should leave Pprof false. When enabled, it
// is only mounted on the dedicated metrics listener (Listen must be
// set) - never on the main S3 listener.
//
// RequireListener defaults to true. A deployment that reports healthy while
// Prometheus silently receives nothing is the worse of the two failures,
// because nothing about it looks wrong until someone goes looking for a graph.
// Dev and embedded use set it false, where the port may well be taken and
// best-effort metrics are fine; it is a pointer so an explicit false is
// distinguishable from an omitted field.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	Listen  string `yaml:"listen"` // Separate listener address (e.g. "127.0.0.1:9091"); if empty, metrics are served on the main listener
	Pprof   bool   `yaml:"pprof"`  // Mount /debug/pprof/* on the metrics listener. Off by default; requires Listen to be set.

	RequireListener *bool `yaml:"require_listener"` // fail startup if the metrics listener cannot bind
}

// ListenerRequired reports whether a metrics bind failure should abort startup.
// Only meaningful when Listen is set; metrics served on the main listener share
// its socket and have nothing separate to fail.
func (m MetricsConfig) ListenerRequired() bool {
	return m.RequireListener == nil || *m.RequireListener
}

// TracingConfig holds OpenTelemetry tracing settings.
type TracingConfig struct {
	Enabled    bool    `yaml:"enabled"`
	Endpoint   string  `yaml:"endpoint"`
	SampleRate float64 `yaml:"sample_rate"`
	Insecure   bool    `yaml:"insecure"` // Use insecure connection (no TLS)
}

// setDefaultsAndValidate sets defaults and validate.
func (t *TelemetryConfig) setDefaultsAndValidate() []error {
	var errs []error

	t.Metrics.Path = cmp.Or(t.Metrics.Path, "/metrics")
	if t.Tracing.SampleRate == 0 && t.Tracing.Enabled {
		t.Tracing.SampleRate = 1.0
	}
	if t.Tracing.Enabled && t.Tracing.Endpoint == "" {
		errs = append(errs, ErrTracingEndpointRequired)
	}

	return errs
}
