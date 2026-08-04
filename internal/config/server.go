// -------------------------------------------------------------------------------
// Server Configuration
//
// Author: Alex Freidah
//
// Defines ServerConfig - the HTTP listener block - plus the TLS, admin
// API, request-id, admission control, and timeout sub-blocks. Carries
// the listen address, optional Unix socket, hard admission cap, load-
// shedding threshold, and read/write/header timeouts. Validators enforce
// that admission limits are non-zero when configured and that the TLS
// cert and key are both present (or both absent) so partial TLS
// configuration cannot start.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	ListenAddr           string        `yaml:"listen_addr"`
	LogLevel             string        `yaml:"log_level"`                // Log level: debug, info, warn, error (default: info)
	MaxObjectSize        int64         `yaml:"max_object_size"`          // Max upload size in bytes (default: 5GB)
	MaxConcurrentRequests int          `yaml:"max_concurrent_requests"`  // Max concurrent S3 requests (default: 1000)
	MaxConcurrentReads    int          `yaml:"max_concurrent_reads"`     // Max concurrent read requests (0 = use global limit)
	MaxConcurrentWrites   int          `yaml:"max_concurrent_writes"`    // Max concurrent write requests (0 = use global limit)
	LoadShedThreshold     float64      `yaml:"load_shed_threshold"`      // Active shedding threshold (0.0-1.0, 0 = disabled)
	AdmissionWait         time.Duration `yaml:"admission_wait"`           // Brief wait before rejection (0 = instant reject)
	BackendTimeout       time.Duration `yaml:"backend_timeout"`          // Per-operation timeout for backend S3 calls (default: 30s)
	ReadHeaderTimeout    time.Duration `yaml:"read_header_timeout"`      // Max time to read request headers (default: 10s)
	ReadTimeout          time.Duration `yaml:"read_timeout"`             // Max time to read entire request including body (default: 5m)
	WriteTimeout         time.Duration `yaml:"write_timeout"`            // Max time to write response (default: 5m)
	IdleTimeout          time.Duration `yaml:"idle_timeout"`             // Max time to wait for next request on keep-alive (default: 120s)
	ShutdownDelay        time.Duration `yaml:"shutdown_delay"`           // Delay before HTTP drain on SIGTERM for LB deregistration (default: 0)
	TLS                  TLSConfig     `yaml:"tls"`
}

// TLSConfig holds optional TLS settings for the HTTP server. When CertFile
// and KeyFile are both set, the server listens with TLS. When both are empty,
// the server runs plain HTTP for backward compatibility.
type TLSConfig struct {
	CertFile     string `yaml:"cert_file"`      // Path to PEM-encoded certificate (or chain)
	KeyFile      string `yaml:"key_file"`       // Path to PEM-encoded private key
	MinVersion   string `yaml:"min_version"`    // Minimum TLS version: "1.2" (default) or "1.3"
	ClientCAFile string `yaml:"client_ca_file"` // Path to CA bundle for client certificate verification (mTLS)
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// setDefaultsAndValidate sets defaults and validate.
func (s *ServerConfig) setDefaultsAndValidate() []error {
	var errs []error
	errs = append(errs, s.validateBasics()...)
	errs = append(errs, s.validateConcurrency()...)
	s.applyTimeoutDefaults()
	errs = append(errs, s.validateTimeouts()...)
	errs = append(errs, s.validateTLS()...)
	return errs
}

// validateBasics checks listen address, log level, and max object size
// defaults.
func (s *ServerConfig) validateBasics() []error {
	var errs []error
	if s.ListenAddr == "" {
		errs = append(errs, fmt.Errorf("server.listen_addr is required"))
	}
	s.LogLevel = cmp.Or(s.LogLevel, "info")
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("%w: %q (want one of debug, info, warn, error)", ErrInvalidLogLevel, s.LogLevel))
	}
	s.MaxObjectSize = cmp.Or(s.MaxObjectSize, 5*1024*1024*1024) // 5 GB
	return errs
}

// validateConcurrency checks concurrency-limit fields and applies the
// combined-request default when all three are zero.
func (s *ServerConfig) validateConcurrency() []error {
	var errs []error
	if s.MaxConcurrentRequests < 0 {
		errs = append(errs, fmt.Errorf("server.max_concurrent_requests must not be negative"))
	}
	if s.MaxConcurrentRequests == 0 && s.MaxConcurrentReads == 0 && s.MaxConcurrentWrites == 0 {
		s.MaxConcurrentRequests = 1000
	}
	if s.MaxConcurrentReads < 0 {
		errs = append(errs, fmt.Errorf("server.max_concurrent_reads must not be negative"))
	}
	if s.MaxConcurrentWrites < 0 {
		errs = append(errs, fmt.Errorf("server.max_concurrent_writes must not be negative"))
	}
	if s.LoadShedThreshold != 0 && (s.LoadShedThreshold < 0 || s.LoadShedThreshold >= 1) {
		errs = append(errs, fmt.Errorf("server.load_shed_threshold must be between 0.0 and 1.0 (exclusive)"))
	}
	if s.AdmissionWait < 0 {
		errs = append(errs, fmt.Errorf("server.admission_wait must not be negative"))
	}
	return errs
}

// applyTimeoutDefaults fills zero-valued timeout fields with production
// defaults.
func (s *ServerConfig) applyTimeoutDefaults() {
	s.BackendTimeout = cmp.Or(s.BackendTimeout, 30*time.Second)
	s.ReadHeaderTimeout = cmp.Or(s.ReadHeaderTimeout, 10*time.Second)
	s.ReadTimeout = cmp.Or(s.ReadTimeout, 5*time.Minute)
	s.WriteTimeout = cmp.Or(s.WriteTimeout, 5*time.Minute)
	s.IdleTimeout = cmp.Or(s.IdleTimeout, 120*time.Second)
}

// validateTimeouts enforces the ordering invariants between the various
// timeout knobs.
func (s *ServerConfig) validateTimeouts() []error {
	var errs []error
	if s.ReadHeaderTimeout > s.ReadTimeout {
		errs = append(errs, fmt.Errorf("server.read_header_timeout must not exceed server.read_timeout"))
	}
	if s.BackendTimeout > s.WriteTimeout {
		errs = append(errs, fmt.Errorf("server.backend_timeout must not exceed server.write_timeout"))
	}
	return errs
}

// validateTLS checks that cert_file and key_file are both set (or both
// unset) and that min_version is a supported value when TLS is on.
func (s *ServerConfig) validateTLS() []error {
	var errs []error
	hasCert := s.TLS.CertFile != ""
	hasKey := s.TLS.KeyFile != ""
	if hasCert != hasKey {
		errs = append(errs, ErrInvalidTLSConfig)
	}
	if hasCert && hasKey {
		s.TLS.MinVersion = cmp.Or(s.TLS.MinVersion, "1.2")
		if s.TLS.MinVersion != "1.2" && s.TLS.MinVersion != "1.3" {
			errs = append(errs, fmt.Errorf("%w: got %q (want %q or %q)", ErrInvalidTLSVersion, s.TLS.MinVersion, "1.2", "1.3"))
		}
	}
	return errs
}
