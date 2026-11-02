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

import (
	"cmp"
	"time"
)

// TelemetryConfig holds observability settings.
type TelemetryConfig struct {
	Metrics MetricsConfig `yaml:"metrics"`
	Tracing TracingConfig `yaml:"tracing"`
}

// MetricsConfig holds Prometheus metrics settings.
//
// Pprof is off by default because the pprof handlers expose runtime state and
// on-demand CPU profiles that double as DoS amplifiers. When enabled it is
// mounted only on the dedicated metrics listener (Listen must be set), never
// on the main S3 listener.
//
// RequireListener defaults to true, so a metrics listener that fails to bind
// fails startup instead of silently dropping metrics. It is a pointer so an
// explicit false is distinguishable from an omitted field.
//
// FleetInterval is how often one instance recomputes the fleet-wide gauges
// and admin status figures from the ledger. Each recompute scans the object
// table, so it runs on its own cadence rather than every usage flush.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	Listen  string `yaml:"listen"` // Separate listener address (e.g. "127.0.0.1:9091"); if empty, metrics are served on the main listener
	Pprof   bool   `yaml:"pprof"`  // Mount /debug/pprof/* on the metrics listener. Off by default; requires Listen to be set.

	RequireListener *bool         `yaml:"require_listener"` // fail startup if the metrics listener cannot bind
	FleetInterval   time.Duration `yaml:"fleet_interval"`   // fleet snapshot recompute interval (default: 60s, minimum 10s)
}

// DefaultFleetInterval and MinFleetInterval bound metrics.fleet_interval.
const (
	DefaultFleetInterval = 60 * time.Second
	MinFleetInterval     = 10 * time.Second
)

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
	t.Metrics.Path = cmp.Or(t.Metrics.Path, "/metrics")
	t.Metrics.FleetInterval = cmp.Or(t.Metrics.FleetInterval, DefaultFleetInterval)
	t.Metrics.RequireListener = new(t.Metrics.ListenerRequired())
	t.Tracing.SampleRate = cmp.Or(t.Tracing.SampleRate, 1.0)
	errs := failed(atLeast("telemetry.metrics.fleet_interval", t.Metrics.FleetInterval, MinFleetInterval))
	if t.Tracing.Enabled {
		errs = append(errs, failed(
			required("telemetry.tracing.endpoint", t.Tracing.Endpoint != ""),
			between("telemetry.tracing.sample_rate", t.Tracing.SampleRate, 0, 1),
		)...)
	}
	return errs
}
