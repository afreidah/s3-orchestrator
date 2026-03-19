// -------------------------------------------------------------------------------
// CertReloader - TLS Certificate Hot-Reload
//
// Author: Alex Freidah
//
// Provides a thread-safe TLS certificate holder that supports hot-reload via
// SIGHUP. The GetCertificate callback is wired into tls.Config so new
// connections pick up rotated certificates without restarting the server.
// -------------------------------------------------------------------------------

package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// -------------------------------------------------------------------------
// CERT RELOADER
// -------------------------------------------------------------------------

// CertReloader holds a TLS certificate that can be atomically swapped on
// reload. Safe for concurrent use by the TLS handshake goroutines and a
// single reloader (SIGHUP handler).
type CertReloader struct {
	certFile string
	keyFile  string
	mu       sync.RWMutex
	cert     *tls.Certificate
}

// NewCertReloader loads the initial certificate from disk and returns a
// reloader ready for use as a tls.Config.GetCertificate callback.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
	}
	return &CertReloader{
		certFile: certFile,
		keyFile:  keyFile,
		cert:     &cert,
	}, nil
}

// GetCertificate returns the current certificate for TLS handshakes. Intended
// as the tls.Config.GetCertificate callback.
func (cr *CertReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cr.mu.RLock()
	defer cr.mu.RUnlock()
	return cr.cert, nil
}

// certExpiryWarningThreshold is how far before expiry we start warning.
const certExpiryWarningThreshold = 24 * time.Hour

// Reload reads the certificate and key from disk and swaps the current
// certificate. If the new files are invalid, the existing certificate is
// preserved and an error is returned. Logs a warning if the leaf certificate
// expires within 24 hours.
func (cr *CertReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(cr.certFile, cr.keyFile)
	if err != nil {
		return fmt.Errorf("failed to reload TLS certificate: %w", err)
	}
	cr.mu.Lock()
	cr.cert = &cert
	cr.mu.Unlock()

	checkCertExpiry(&cert, cr.certFile)

	slog.Info("TLS certificate reloaded", "cert_file", cr.certFile) //nolint:sloglint // Reload() has no context
	return nil
}

// checkCertExpiry parses the leaf certificate and logs a warning if it expires
// within certExpiryWarningThreshold.
func checkCertExpiry(cert *tls.Certificate, certFile string) {
	if cert.Leaf == nil {
		// tls.LoadX509KeyPair does not populate Leaf; parse it ourselves.
		if len(cert.Certificate) > 0 {
			parsed, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				slog.Warn("Failed to parse TLS leaf certificate for expiry check", //nolint:sloglint // no context available
					"cert_file", certFile, "error", err)
				return
			}
			cert.Leaf = parsed
		}
	}
	if cert.Leaf != nil {
		remaining := time.Until(cert.Leaf.NotAfter)
		if remaining < certExpiryWarningThreshold {
			slog.Warn("TLS certificate expires soon", //nolint:sloglint // no context available
				"cert_file", certFile,
				"expires_at", cert.Leaf.NotAfter,
				"remaining", remaining.Truncate(time.Minute))
		}
	}
}
