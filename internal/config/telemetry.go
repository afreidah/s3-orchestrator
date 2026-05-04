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

// TelemetryConfig holds observability settings.
type TelemetryConfig struct {
	Metrics MetricsConfig `yaml:"metrics"`
	Tracing TracingConfig `yaml:"tracing"`
}

// MetricsConfig holds Prometheus metrics settings.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
	Listen  string `yaml:"listen"` // Separate listener address (e.g. "127.0.0.1:9091"); if empty, metrics are served on the main listener
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

	if t.Metrics.Path == "" {
		t.Metrics.Path = "/metrics"
	}
	if t.Tracing.SampleRate == 0 && t.Tracing.Enabled {
		t.Tracing.SampleRate = 1.0
	}
	if t.Tracing.Enabled && t.Tracing.Endpoint == "" {
		errs = append(errs, ErrTracingEndpointRequired)
	}

	return errs
}
