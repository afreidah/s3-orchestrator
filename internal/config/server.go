// -------------------------------------------------------------------------------
// Server Configuration
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package config

import "time"

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

func (s *ServerConfig) setDefaultsAndValidate() []string {
	var errs []string

	if s.ListenAddr == "" {
		errs = append(errs, "server.listen_addr is required")
	}

	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, "server.log_level must be one of: debug, info, warn, error")
	}

	if s.MaxObjectSize == 0 {
		s.MaxObjectSize = 5 * 1024 * 1024 * 1024 // 5 GB
	}

	if s.MaxConcurrentRequests < 0 {
		errs = append(errs, "server.max_concurrent_requests must not be negative")
	}
	if s.MaxConcurrentRequests == 0 && s.MaxConcurrentReads == 0 && s.MaxConcurrentWrites == 0 {
		s.MaxConcurrentRequests = 1000
	}
	if s.MaxConcurrentReads < 0 {
		errs = append(errs, "server.max_concurrent_reads must not be negative")
	}
	if s.MaxConcurrentWrites < 0 {
		errs = append(errs, "server.max_concurrent_writes must not be negative")
	}
	if s.LoadShedThreshold < 0 || s.LoadShedThreshold >= 1 {
		if s.LoadShedThreshold != 0 {
			errs = append(errs, "server.load_shed_threshold must be between 0.0 and 1.0 (exclusive)")
		}
	}
	if s.AdmissionWait < 0 {
		errs = append(errs, "server.admission_wait must not be negative")
	}

	if s.BackendTimeout == 0 {
		s.BackendTimeout = 30 * time.Second
	}
	if s.ReadHeaderTimeout == 0 {
		s.ReadHeaderTimeout = 10 * time.Second
	}
	if s.ReadTimeout == 0 {
		s.ReadTimeout = 5 * time.Minute
	}
	if s.WriteTimeout == 0 {
		s.WriteTimeout = 5 * time.Minute
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = 120 * time.Second
	}

	if s.ReadHeaderTimeout > s.ReadTimeout {
		errs = append(errs, "server.read_header_timeout must not exceed server.read_timeout")
	}
	if s.BackendTimeout > s.WriteTimeout {
		errs = append(errs, "server.backend_timeout must not exceed server.write_timeout")
	}

	// TLS validation
	hasCert := s.TLS.CertFile != ""
	hasKey := s.TLS.KeyFile != ""
	if hasCert != hasKey {
		errs = append(errs, "server.tls requires both cert_file and key_file")
	}
	if hasCert && hasKey {
		if s.TLS.MinVersion == "" {
			s.TLS.MinVersion = "1.2"
		}
		if s.TLS.MinVersion != "1.2" && s.TLS.MinVersion != "1.3" {
			errs = append(errs, "server.tls.min_version must be \"1.2\" or \"1.3\"")
		}
	}

	return errs
}
