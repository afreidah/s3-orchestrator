// -------------------------------------------------------------------------------
// Server Configuration
//
// Author: Alex Freidah
//
// Defines ServerConfig - the HTTP listener block - plus the TLS, admin
// API, request-id, admission control, and timeout sub-blocks. Carries
// the listen address, optional Unix socket, hard admission cap, load-
// shedding threshold, request-header limits, and read/write/header
// timeouts. Validators enforce
// that admission limits are non-zero when configured and that the TLS
// cert and key are both present (or both absent) so partial TLS
// configuration cannot start.
// -------------------------------------------------------------------------------

package config

import (
	"cmp"
	"fmt"
	"net/http"
	"os"
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	ListenAddr            string        `yaml:"listen_addr"`
	LogLevel              string        `yaml:"log_level"`               // Log level: debug, info, warn, error (default: info)
	MaxObjectSize         int64         `yaml:"max_object_size"`         // Max upload size in bytes (default: 5GB)
	SpillDir              string        `yaml:"spill_dir"`               // Directory for large-payload spill files (default: OS temp dir)
	MaxConcurrentRequests int           `yaml:"max_concurrent_requests"` // Max concurrent S3 requests (default: 1000)
	MaxConcurrentReads    int           `yaml:"max_concurrent_reads"`    // Max concurrent read requests; splits the pools (0 = global limit)
	MaxConcurrentWrites   int           `yaml:"max_concurrent_writes"`   // Max concurrent write requests; splits the pools (0 = global limit)
	MaxHeaderBytes        int           `yaml:"max_header_bytes"`        // Max total request header size in bytes (default: 1MB)
	MaxHeaderValueCount   int           `yaml:"max_header_value_count"`  // Max number of header values per request (default: 500)
	LoadShedThreshold     float64       `yaml:"load_shed_threshold"`     // Active shedding threshold (0.0-1.0, 0 = disabled)
	AdmissionWait         time.Duration `yaml:"admission_wait"`          // Brief wait before rejection (0 = instant reject)
	BackendTimeout        time.Duration `yaml:"backend_timeout"`         // Per-operation timeout for backend S3 calls (default: 30s)
	ReadHeaderTimeout     time.Duration `yaml:"read_header_timeout"`     // Max time to read request headers (default: 10s)
	ReadTimeout           time.Duration `yaml:"read_timeout"`            // Max time to read entire request including body (default: 5m)
	WriteTimeout          time.Duration `yaml:"write_timeout"`           // Max time to write response (default: 5m)
	IdleTimeout           time.Duration `yaml:"idle_timeout"`            // Max time to wait for next request on keep-alive (default: 120s)
	ShutdownDelay         time.Duration `yaml:"shutdown_delay"`          // Delay before HTTP drain on SIGTERM for LB deregistration (default: 0)
	TLS                   TLSConfig     `yaml:"tls"`
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

// DefaultMaxConcurrentRequests is the shared admission pool when none of the
// three limits is set, and the size of a pool a split leaves unset.
const DefaultMaxConcurrentRequests = 1000

// AdmissionPools is the concurrency the S3 listener admits: one shared pool,
// or a read pool and a write pool when the operator set either of those. A
// zero Shared admits without limit, which only a config built in code has.
type AdmissionPools struct {
	Shared int
	Reads  int
	Writes int
}

// Split reports whether reads and writes are admitted from pools of their own.
func (a AdmissionPools) Split() bool {
	return a.Reads > 0
}

// WriteCapacity is how many writes the pools admit at once, which the
// background workers share in split mode.
func (a AdmissionPools) WriteCapacity() int {
	if a.Split() {
		return a.Writes
	}
	return a.Shared
}

// Admission decides the admission pools. Setting either split limit splits the
// pools, and the one left unset takes max_concurrent_requests, or the default
// when that is unset too, so setting one never turns admission off.
func (s *ServerConfig) Admission() AdmissionPools {
	if s.MaxConcurrentReads == 0 && s.MaxConcurrentWrites == 0 {
		return AdmissionPools{Shared: s.MaxConcurrentRequests}
	}
	return AdmissionPools{
		Reads:  cmp.Or(s.MaxConcurrentReads, s.MaxConcurrentRequests, DefaultMaxConcurrentRequests),
		Writes: cmp.Or(s.MaxConcurrentWrites, s.MaxConcurrentRequests, DefaultMaxConcurrentRequests),
	}
}

// -------------------------------------------------------------------------
// VALIDATION
// -------------------------------------------------------------------------

// setDefaultsAndValidate sets defaults and validate.
func (s *ServerConfig) setDefaultsAndValidate() []error {
	var errs []error
	errs = append(errs, s.validateBasics()...)
	errs = append(errs, s.validateConcurrency()...)
	errs = append(errs, s.validateHeaderLimits()...)
	s.applyTimeoutDefaults()
	errs = append(errs, s.validateTimeouts()...)
	errs = append(errs, s.validateTLS()...)
	return errs
}

// validateBasics checks listen address, log level, and max object size
// defaults.
func (s *ServerConfig) validateBasics() []error {
	s.LogLevel = cmp.Or(s.LogLevel, "info")
	s.MaxObjectSize = cmp.Or(s.MaxObjectSize, 5*1024*1024*1024) // 5 GB
	errs := failed(
		required("server.listen_addr", s.ListenAddr != ""),
		positive("server.max_object_size", s.MaxObjectSize),
	)
	if _, err := ParseLogLevel(s.LogLevel); err != nil {
		errs = append(errs, err)
	}
	return append(errs, s.validateSpillDir()...)
}

// validateSpillDir checks that a configured spill directory exists and is one.
// Checked at startup because the alternative is discovering the typo on the
// first object too large to hold in memory, which is the worst moment for a
// write to start failing.
func (s *ServerConfig) validateSpillDir() []error {
	if s.SpillDir == "" {
		return nil
	}
	info, err := os.Stat(s.SpillDir)
	if err != nil {
		return []error{fmt.Errorf("%w: %s: %w", ErrSpillDirUnusable, s.SpillDir, err)}
	}
	if !info.IsDir() {
		return []error{fmt.Errorf("%w: %s is not a directory", ErrSpillDirUnusable, s.SpillDir)}
	}
	return nil
}

// validateConcurrency checks concurrency-limit fields and applies the
// combined-request default when all three are zero.
func (s *ServerConfig) validateConcurrency() []error {
	errs := failed(
		nonNegative("server.max_concurrent_requests", s.MaxConcurrentRequests),
		nonNegative("server.max_concurrent_reads", s.MaxConcurrentReads),
		nonNegative("server.max_concurrent_writes", s.MaxConcurrentWrites),
		nonNegative("server.admission_wait", s.AdmissionWait),
	)
	if s.MaxConcurrentRequests == 0 && s.MaxConcurrentReads == 0 && s.MaxConcurrentWrites == 0 {
		s.MaxConcurrentRequests = DefaultMaxConcurrentRequests
	}
	if s.LoadShedThreshold != 0 && (s.LoadShedThreshold < 0 || s.LoadShedThreshold >= 1) {
		errs = append(errs, fmt.Errorf("server.load_shed_threshold %w: want 0 (off), or above 0 and below 1 (got %v)",
			ErrOutOfRange, s.LoadShedThreshold))
	}
	return errs
}

// validateHeaderLimits applies the net/http defaults to the request-header
// limits and rejects negative values. The defaults are pinned to the stdlib
// constants rather than literals so a future Go release that retunes them
// carries through instead of silently diverging from unconfigured servers.
func (s *ServerConfig) validateHeaderLimits() []error {
	errs := failed(
		nonNegative("server.max_header_bytes", s.MaxHeaderBytes),
		nonNegative("server.max_header_value_count", s.MaxHeaderValueCount),
	)
	s.MaxHeaderBytes = cmp.Or(s.MaxHeaderBytes, http.DefaultMaxHeaderBytes)
	s.MaxHeaderValueCount = cmp.Or(s.MaxHeaderValueCount, http.DefaultMaxHeaderValueCount)
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

// validateTimeouts refuses negative timeouts and enforces the ordering
// invariants between them.
func (s *ServerConfig) validateTimeouts() []error {
	errs := failed(
		positive("server.backend_timeout", s.BackendTimeout),
		positive("server.read_header_timeout", s.ReadHeaderTimeout),
		positive("server.read_timeout", s.ReadTimeout),
		positive("server.write_timeout", s.WriteTimeout),
		positive("server.idle_timeout", s.IdleTimeout),
		nonNegative("server.shutdown_delay", s.ShutdownDelay),
	)
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
