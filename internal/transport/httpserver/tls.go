// -------------------------------------------------------------------------------
// HTTP Server - TLS Configuration
//
// Author: Alex Freidah
//
// Builds a *tls.Config for the daemon's HTTP listener. The certificate reloader
// is owned by this package so SIGHUP-driven reloads can fan out to a single
// reload point without the reload coordinator reaching into the listener
// internals. mTLS is configured when ClientCAFile is set; otherwise the
// returned config has the default ClientAuth (NoClientCert).
// -------------------------------------------------------------------------------

package httpserver

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
)

// buildTLSConfig constructs the listener's *tls.Config and its CertReloader.
// Returns (nil, nil, nil) when TLS is not configured. The reloader is
// returned separately so the reload coordinator can call CertReloader.Reload
// without reaching into the listener.
func buildTLSConfig(cfg *config.TLSConfig) (*tls.Config, *httputil.CertReloader, error) {
	if cfg.CertFile == "" {
		return nil, nil, nil
	}

	reloader, err := httputil.NewCertReloader(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("load TLS certificate: %w", err)
	}

	tlsCfg := &tls.Config{ //nolint:gosec // G402: MinVersion set from config, defaults to TLS 1.2
		GetCertificate: reloader.GetCertificate,
		MinVersion:     parseTLSVersion(cfg.MinVersion),
	}

	if cfg.ClientCAFile != "" {
		caCert, err := os.ReadFile(cfg.ClientCAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read client CA file: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caCert) {
			return nil, nil, fmt.Errorf("parse client CA certificate: no valid PEM blocks found")
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = caPool
	}

	return tlsCfg, reloader, nil
}

// parseTLSVersion maps a config string to a tls.VersionTLS constant.
// Unrecognized values fall back to TLS 1.2.
func parseTLSVersion(v string) uint16 {
	switch v {
	case "1.3":
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}
