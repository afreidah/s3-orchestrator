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
	"fmt"
	"log/slog"
	"sync"
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

// Reload reads the certificate and key from disk and swaps the current
// certificate. If the new files are invalid, the existing certificate is
// preserved and an error is returned.
func (cr *CertReloader) Reload() error {
	cert, err := tls.LoadX509KeyPair(cr.certFile, cr.keyFile)
	if err != nil {
		return fmt.Errorf("failed to reload TLS certificate: %w", err)
	}
	cr.mu.Lock()
	cr.cert = &cert
	cr.mu.Unlock()
	slog.Info("TLS certificate reloaded", "cert_file", cr.certFile)
	return nil
}
