// -------------------------------------------------------------------------------
// Configuration - S3 Proxy Settings
//
// Project: Munchbox / Author: Alex Freidah
//
// Configuration types and loader for the S3 proxy. Supports environment variable
// expansion in YAML values using ${VAR} syntax. Validates required fields before
// returning to catch misconfiguration early.
// -------------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// -------------------------------------------------------------------------
// CONFIGURATION TYPES
// -------------------------------------------------------------------------

// Config holds the complete service configuration.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Auth      AuthConfig      `yaml:"auth"`
	Backend   BackendConfig   `yaml:"backend"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	ListenAddr    string `yaml:"listen_addr"`
	VirtualBucket string `yaml:"virtual_bucket"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	Token string `yaml:"token"`
}

// BackendConfig holds the single backend configuration for Phase 0.
type BackendConfig struct {
	Name            string `yaml:"name"` // Identifier for metrics/tracing
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	ForcePathStyle  bool   `yaml:"force_path_style"`
}

// TelemetryConfig holds observability settings.
type TelemetryConfig struct {
	Metrics MetricsConfig `yaml:"metrics"`
	Tracing TracingConfig `yaml:"tracing"`
}

// MetricsConfig holds Prometheus metrics settings.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// TracingConfig holds OpenTelemetry tracing settings.
type TracingConfig struct {
	Enabled    bool    `yaml:"enabled"`
	Endpoint   string  `yaml:"endpoint"`
	SampleRate float64 `yaml:"sample_rate"`
}

// -------------------------------------------------------------------------
// CONFIGURATION LOADER
// -------------------------------------------------------------------------

// LoadConfig reads and parses the configuration file with environment variable
// expansion. Returns an error if the file cannot be read, parsed, or validated.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// --- Expand environment variables ---
	expanded := os.Expand(string(data), func(key string) string {
		return os.Getenv(key)
	})

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// Validate checks that required configuration values are present and sets defaults.
func (c *Config) Validate() error {
	var errors []string

	if c.Server.ListenAddr == "" {
		errors = append(errors, "server.listen_addr is required")
	}
	if c.Server.VirtualBucket == "" {
		errors = append(errors, "server.virtual_bucket is required")
	}
	if c.Backend.Endpoint == "" {
		errors = append(errors, "backend.endpoint is required")
	}
	if c.Backend.Bucket == "" {
		errors = append(errors, "backend.bucket is required")
	}
	if c.Backend.AccessKeyID == "" {
		errors = append(errors, "backend.access_key_id is required")
	}
	if c.Backend.SecretAccessKey == "" {
		errors = append(errors, "backend.secret_access_key is required")
	}

	// --- Set defaults ---
	if c.Backend.Name == "" {
		c.Backend.Name = "default"
	}
	if c.Telemetry.Metrics.Path == "" {
		c.Telemetry.Metrics.Path = "/metrics"
	}
	if c.Telemetry.Tracing.SampleRate == 0 && c.Telemetry.Tracing.Enabled {
		c.Telemetry.Tracing.SampleRate = 1.0
	}

	// --- Validate tracing config ---
	if c.Telemetry.Tracing.Enabled && c.Telemetry.Tracing.Endpoint == "" {
		errors = append(errors, "telemetry.tracing.endpoint is required when tracing is enabled")
	}

	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}
