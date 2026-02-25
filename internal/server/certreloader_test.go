// -------------------------------------------------------------------------------
// CertReloader Tests - TLS Certificate Hot-Reload
//
// Author: Alex Freidah
//
// Unit tests for the CertReloader covering initial load, GetCertificate
// callback, successful reload, and error handling when cert files are invalid.
// -------------------------------------------------------------------------------

package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

func TestNewCertReloader_ValidCert(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}
	if cr.cert == nil {
		t.Fatal("certificate should not be nil")
	}
}

func TestNewCertReloader_InvalidPath(t *testing.T) {
	_, err := NewCertReloader("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Fatal("expected error for nonexistent files")
	}
}

func TestCertReloader_GetCertificate(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}

	cert, err := cr.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate failed: %v", err)
	}
	if cert == nil {
		t.Fatal("GetCertificate returned nil")
	}
}

func TestCertReloader_Reload(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}

	origCert, _ := cr.GetCertificate(nil)

	// Write a new cert to the same files
	writeTestCert(t, certFile, keyFile)

	if err := cr.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	newCert, _ := cr.GetCertificate(nil)
	if origCert == newCert {
		t.Error("certificate pointer should change after reload")
	}
}

func TestCertReloader_ReloadBadCert_KeepsOld(t *testing.T) {
	certFile, keyFile := generateTestCert(t)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}

	origCert, _ := cr.GetCertificate(nil)

	// Corrupt the cert file
	if err := os.WriteFile(certFile, []byte("not a cert"), 0644); err != nil {
		t.Fatalf("failed to corrupt cert: %v", err)
	}

	if err := cr.Reload(); err == nil {
		t.Fatal("expected error for corrupt cert")
	}

	// Original cert should still be served
	currentCert, _ := cr.GetCertificate(nil)
	if currentCert != origCert {
		t.Error("certificate should be preserved after failed reload")
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// generateTestCert creates a self-signed ECDSA certificate and key in temp
// files. Returns the file paths. Files are cleaned up by t.TempDir().
func generateTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	writeTestCert(t, certFile, keyFile)
	return certFile, keyFile
}

// writeTestCert generates a fresh self-signed certificate and writes it to
// the given paths.
func writeTestCert(t *testing.T, certFile, keyFile string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(certFile, certPEM, 0644); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
}
