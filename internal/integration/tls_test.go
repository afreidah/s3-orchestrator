//go:build integration

// -------------------------------------------------------------------------------
// TLS Integration Tests - End-to-End TLS and mTLS Verification
//
// Author: Alex Freidah
//
// Integration tests for TLS termination, mTLS client certificate verification,
// and hot-reload of certificates via CertReloader. Shares the same backend
// manager and store as other integration tests.
// -------------------------------------------------------------------------------

package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/server"
)

// -------------------------------------------------------------------------
// TLS
// -------------------------------------------------------------------------

func TestTLS(t *testing.T) {
	ctx := context.Background()

	t.Run("CRUDOverTLS", func(t *testing.T) {
		certs := generateTLSCerts(t)
		addr := startTLSProxy(t, certs.ServerCertFile, certs.ServerKeyFile, "")
		client := newTLSS3Client(t, addr, certs.CACertPEM)

		key := uniqueKey(t, "tls-crud")
		body := []byte("hello-tls")

		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("PutObject over TLS: %v", err)
		}

		resp, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject over TLS: %v", err)
		}
		defer resp.Body.Close()

		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body mismatch: got %q, want %q", got, body)
		}
	})

	t.Run("mTLSAcceptsValidClient", func(t *testing.T) {
		certs := generateTLSCerts(t)
		addr := startTLSProxy(t, certs.ServerCertFile, certs.ServerKeyFile, certs.CACertFile)
		client := newMTLSS3Client(t, addr, certs.CACertPEM, certs.ClientCertFile, certs.ClientKeyFile)

		key := uniqueKey(t, "mtls")
		body := []byte("mtls-ok")

		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("PutObject with valid client cert: %v", err)
		}

		resp, err := client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject with valid client cert: %v", err)
		}
		defer resp.Body.Close()

		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body mismatch: got %q, want %q", got, body)
		}
	})

	t.Run("mTLSRejectsNoClientCert", func(t *testing.T) {
		certs := generateTLSCerts(t)
		addr := startTLSProxy(t, certs.ServerCertFile, certs.ServerKeyFile, certs.CACertFile)

		// Client trusts server CA but does NOT present a client cert
		client := newTLSS3Client(t, addr, certs.CACertPEM)

		_, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(uniqueKey(t, "mtls-reject")),
			Body:          bytes.NewReader([]byte("x")),
			ContentLength: aws.Int64(1),
		})
		if err == nil {
			t.Fatal("expected TLS handshake failure without client cert, got nil")
		}
	})

	t.Run("CertReloaderIntegration", func(t *testing.T) {
		certs := generateTLSCerts(t)

		// Start proxy using CertReloader (same code path as production main.go)
		reloader, err := server.NewCertReloader(certs.ServerCertFile, certs.ServerKeyFile)
		if err != nil {
			t.Fatalf("NewCertReloader: %v", err)
		}

		srv := &server.Server{
			Manager: testManager,
		}
		srv.SetBucketAuth(auth.NewBucketRegistry([]config.BucketConfig{{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{{
				AccessKeyID:    "test",
				SecretAccessKey: "test",
			}},
		}}))

		tlsCfg := &tls.Config{
			GetCertificate: reloader.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}

		listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
		if err != nil {
			t.Fatalf("TLS listen: %v", err)
		}

		httpServer := &http.Server{
			Handler:      srv,
			ReadTimeout:  5 * time.Minute,
			WriteTimeout: 5 * time.Minute,
		}
		go httpServer.Serve(listener)
		t.Cleanup(func() { httpServer.Shutdown(context.Background()) })

		addr := listener.Addr().String()
		client := newTLSS3Client(t, addr, certs.CACertPEM)

		// Verify initial cert works
		key := uniqueKey(t, "tls-reload")
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader([]byte("before-reload")),
			ContentLength: aws.Int64(13),
		})
		if err != nil {
			t.Fatalf("PutObject before reload: %v", err)
		}

		// Regenerate server cert from same CA, write to same files
		rewriteServerCert(t, certs)

		if err := reloader.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}

		// Force a new TCP connection so the reloaded cert is used
		client = newTLSS3Client(t, addr, certs.CACertPEM)

		key2 := uniqueKey(t, "tls-reload-after")
		_, err = client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(key2),
			Body:          bytes.NewReader([]byte("after-reload")),
			ContentLength: aws.Int64(12),
		})
		if err != nil {
			t.Fatalf("PutObject after reload: %v", err)
		}
	})
}

// -------------------------------------------------------------------------
// TLS HELPERS
// -------------------------------------------------------------------------

// tlsTestCerts holds generated TLS certificates for integration tests.
type tlsTestCerts struct {
	CACertPEM      []byte
	CACertFile     string
	caKey          *ecdsa.PrivateKey
	caCert         *x509.Certificate
	ServerCertFile string
	ServerKeyFile  string
	ClientCertFile string
	ClientKeyFile  string
}

// generateTLSCerts creates a CA, server certificate, and client certificate
// in a temporary directory. All certs are signed by the CA.
func generateTLSCerts(t *testing.T) *tlsTestCerts {
	t.Helper()
	dir := t.TempDir()

	// --- CA ---
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	caCertFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caCertFile, caCertPEM, 0644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}

	certs := &tlsTestCerts{
		CACertPEM:  caCertPEM,
		CACertFile: caCertFile,
		caKey:      caKey,
		caCert:     caCert,
	}

	// --- Server cert ---
	certs.ServerCertFile = filepath.Join(dir, "server-cert.pem")
	certs.ServerKeyFile = filepath.Join(dir, "server-key.pem")
	writeSignedCert(t, caCert, caKey, certs.ServerCertFile, certs.ServerKeyFile,
		[]string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)},
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 2)

	// --- Client cert ---
	certs.ClientCertFile = filepath.Join(dir, "client-cert.pem")
	certs.ClientKeyFile = filepath.Join(dir, "client-key.pem")
	writeSignedCert(t, caCert, caKey, certs.ClientCertFile, certs.ClientKeyFile,
		nil, nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, 3)

	return certs
}

// writeSignedCert generates an ECDSA key pair and a certificate signed by the
// given CA, then writes the cert and key to the specified files.
func writeSignedCert(t *testing.T, caCert *x509.Certificate, caKey *ecdsa.PrivateKey,
	certFile, keyFile string, dnsNames []string, ips []net.IP,
	extKeyUsage []x509.ExtKeyUsage, serial int64) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  extKeyUsage,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// rewriteServerCert regenerates the server cert from the same CA, simulating
// a certificate rotation (e.g. Vault PKI renewal).
func rewriteServerCert(t *testing.T, certs *tlsTestCerts) {
	t.Helper()
	writeSignedCert(t, certs.caCert, certs.caKey,
		certs.ServerCertFile, certs.ServerKeyFile,
		[]string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)},
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 100)
}

// startTLSProxy starts an ephemeral TLS-enabled proxy sharing the global
// testManager. When clientCAFile is non-empty, mTLS is required.
// Returns the listener address. The server is stopped via t.Cleanup.
func startTLSProxy(t *testing.T, certFile, keyFile, clientCAFile string) string {
	t.Helper()

	srv := &server.Server{
		Manager: testManager,
	}
	srv.SetBucketAuth(auth.NewBucketRegistry([]config.BucketConfig{{
		Name: virtualBucket,
		Credentials: []config.CredentialConfig{{
			AccessKeyID:    "test",
			SecretAccessKey: "test",
		}},
	}}))

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if clientCAFile != "" {
		caPEM, err := os.ReadFile(clientCAFile)
		if err != nil {
			t.Fatalf("read client CA: %v", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			t.Fatal("failed to add client CA to pool")
		}
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
		tlsCfg.ClientCAs = caPool
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("TLS listen: %v", err)
	}

	httpServer := &http.Server{
		Handler:      srv,
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 5 * time.Minute,
	}
	go httpServer.Serve(listener)
	t.Cleanup(func() { httpServer.Shutdown(context.Background()) })

	return listener.Addr().String()
}

// newTLSS3Client returns an S3 client that connects over TLS, trusting
// the given CA certificate.
func newTLSS3Client(t *testing.T, addr string, caCertPEM []byte) *s3.Client {
	t.Helper()

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("failed to add CA cert to pool")
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    caPool,
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	return s3.New(s3.Options{
		BaseEndpoint: aws.String("https://" + addr),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})
}

// newMTLSS3Client returns an S3 client that connects over TLS with a client
// certificate for mutual TLS authentication.
func newMTLSS3Client(t *testing.T, addr string, caCertPEM []byte, clientCertFile, clientKeyFile string) *s3.Client {
	t.Helper()

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("failed to add CA cert to pool")
	}

	clientCert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
	if err != nil {
		t.Fatalf("load client cert: %v", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      caPool,
				Certificates: []tls.Certificate{clientCert},
				MinVersion:   tls.VersionTLS12,
			},
		},
	}

	return s3.New(s3.Options{
		BaseEndpoint: aws.String("https://" + addr),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
		HTTPClient:   httpClient,
	})
}
