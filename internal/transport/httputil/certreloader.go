// -------------------------------------------------------------------------------
// CertReloader - TLS Certificate Hot-Reload
//
// Author: Alex Freidah
//
// Provides a thread-safe TLS certificate holder that supports hot-reload via
// SIGHUP. The GetCertificate callback is wired into tls.Config so new
// connections pick up rotated certificates without restarting the server.
// -------------------------------------------------------------------------------

package httputil

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
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
	log      *slog.Logger
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
		log:      slog.Default().With(logfmt.Component("cert_reloader")),
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

	checkCertExpiry(cr.log, &cert, cr.certFile)

	cr.log.InfoContext(context.Background(), "TLS certificate reloaded", "cert_file", cr.certFile)
	return nil
}

// checkCertExpiry logs a warning if the leaf certificate expires within
// certExpiryWarningThreshold. tls.LoadX509KeyPair fills in Leaf itself.
func checkCertExpiry(log *slog.Logger, cert *tls.Certificate, certFile string) {
	if cert.Leaf == nil {
		return
	}
	if remaining := time.Until(cert.Leaf.NotAfter); remaining < certExpiryWarningThreshold {
		log.WarnContext(context.Background(), "TLS certificate expires soon",
			"cert_file", certFile,
			"expires_at", cert.Leaf.NotAfter,
			"remaining", remaining.Truncate(time.Minute))
	}
}
