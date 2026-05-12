// -------------------------------------------------------------------------------
// CertReloader Tests - TLS Certificate Hot-Reload
//
// Author: Alex Freidah
//
// Unit tests for the CertReloader covering initial load, GetCertificate
// callback, successful reload, and error handling when cert files are invalid.
// -------------------------------------------------------------------------------

package httputil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestNewCertReloader_ValidCert verifies the new cert reloader valid cert contract.
// Asserts that NewCertReloader failed:.
func TestNewCertReloader_ValidCert(t *testing.T) {
	t.Parallel()
	certFile, keyFile := generateTestCert(t)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}
	if cr.cert == nil {
		t.Fatal("certificate should not be nil")
	}
}

// TestNewCertReloader_InvalidPath verifies the new cert reloader invalid path behaviour described by the test name.
func TestNewCertReloader_InvalidPath(t *testing.T) {
	t.Parallel()
	_, err := NewCertReloader("/nonexistent/cert.pem", "/nonexistent/key.pem")
	if err == nil {
		t.Fatal("expected error for nonexistent files")
	}
}

// TestCertReloader_GetCertificate verifies the cert reloader get certificate contract.
// Asserts that NewCertReloader failed:.
func TestCertReloader_GetCertificate(t *testing.T) {
	t.Parallel()
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

// TestCertReloader_Reload verifies the cert reloader reload contract.
// Asserts that NewCertReloader failed:.
func TestCertReloader_Reload(t *testing.T) {
	t.Parallel()
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

// TestCertReloader_ReloadBadCert_KeepsOld verifies the cert reloader reload bad cert keeps old contract.
// Asserts that NewCertReloader failed:.
func TestCertReloader_ReloadBadCert_KeepsOld(t *testing.T) {
	t.Parallel()
	certFile, keyFile := generateTestCert(t)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader failed: %v", err)
	}

	origCert, _ := cr.GetCertificate(nil)

	// Corrupt the cert file
	if err := os.WriteFile(certFile, []byte("not a cert"), 0600); err != nil {
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

// TestCertReloader_ReloadWarnsOnExpiringSoon verifies the cert reloader reload warns on expiring soon contract.
// Asserts that NewCertReloader:.
func TestCertReloader_ReloadWarnsOnExpiringSoon(t *testing.T) {
	t.Parallel()
	// Generate a cert that expires in 30 minutes (within the 24h threshold)
	certFile, keyFile := generateTestCertWithExpiry(t, 30*time.Minute)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	// Reload should succeed (warning is logged but not an error)
	if err := cr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
}

// TestCheckCertExpiry_BadLeafBytes drives the leaf-parse-failed branch:
// when cert.Leaf is nil and cert.Certificate[0] is not a valid DER
// payload, x509.ParseCertificate errors and the helper logs the failure
// rather than panicking.
func TestCheckCertExpiry_BadLeafBytes(t *testing.T) {
	t.Parallel()
	cert := &tls.Certificate{
		Certificate: [][]byte{[]byte("not a valid DER cert")},
	}
	// Should not panic; log goes to slog.Default().
	checkCertExpiry(slog.Default(), cert, "fixture")
}

// TestCheckCertExpiry_LongLived verifies the check cert expiry long lived contract.
// Asserts that NewCertReloader:.
func TestCheckCertExpiry_LongLived(t *testing.T) {
	t.Parallel()
	// A cert with 30 days remaining should not trigger a warning.
	// We just verify it doesn't panic; the warning is only logged, not returned.
	certFile, keyFile := generateTestCertWithExpiry(t, 30*24*time.Hour)
	cr, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	// Reload parses and checks; no warning expected
	if err := cr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
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

// generateTestCertWithExpiry creates a self-signed cert that expires after
// the given duration.
func generateTestCertWithExpiry(t *testing.T, validity time.Duration) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(validity),
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

	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
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

	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatalf("failed to write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}
}
