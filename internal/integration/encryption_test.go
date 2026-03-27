// -------------------------------------------------------------------------------
// Integration Tests - Encrypt/Decrypt Existing Objects
//
// Author: Alex Freidah
//
// Integration tests covering the full encrypt-existing and decrypt-existing admin
// API round-trip. Verifies that plaintext objects can be encrypted in-place,
// remain readable through the proxy, and can be decrypted back to plaintext with
// correct DB metadata transitions and quota tracking at each stage.
// -------------------------------------------------------------------------------

//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/afreidah/s3-orchestrator/internal/admin"
	"github.com/afreidah/s3-orchestrator/internal/auth"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/proxy"
	"github.com/afreidah/s3-orchestrator/internal/server"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

// testMasterKey is a 256-bit AES key used for integration test encryption.
var testMasterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("K"), 32))

const adminToken = "test-admin-token"

// encryptionTestEnv holds the components needed for encryption integration tests.
type encryptionTestEnv struct {
	proxyClient *s3.Client
	adminAddr   string
	encryptor   *encryption.Encryptor
	rawStore    *store.Store
	manager     *proxy.BackendManager
}

// setupEncryptionEnv creates a fresh BackendManager with encryption enabled,
// an admin API server, and a proxy server. Returns the environment and a
// cleanup function.
func setupEncryptionEnv(t *testing.T) *encryptionTestEnv {
	t.Helper()
	resetState(t)

	ctx := context.Background()

	// Create encryptor with a test key
	keyProvider, err := encryption.NewConfigKeyProvider(testMasterKey, "test-key")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	enc, err := encryption.NewEncryptor(keyProvider, 65536)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	// Build a manager with encryption enabled using the same backends/store
	cbStore := store.NewCircuitBreakerStore(testStore, config.CircuitBreakerConfig{
		FailureThreshold: 3,
		OpenTimeout:      500 * time.Millisecond,
		CacheTTL:         60 * time.Second,
	})

	mgr := proxy.NewBackendManager(&proxy.BackendManagerConfig{
		Backends:        testBackends,
		Store:           cbStore,
		Order:           testBackendOrder,
		CacheTTL:        60 * time.Second,
		BackendTimeout:  30 * time.Second,
		RoutingStrategy: "pack",
		Encryptor:       enc,
	})

	// Start proxy server
	srv := &server.Server{Manager: mgr}
	srv.SetBucketAuth(auth.NewBucketRegistry([]config.BucketConfig{
		{
			Name: virtualBucket,
			Credentials: []config.CredentialConfig{
				{AccessKeyID: "test", SecretAccessKey: "test"},
			},
		},
	}))

	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	proxyServer := &http.Server{Handler: srv}
	go proxyServer.Serve(proxyListener)
	t.Cleanup(func() { proxyServer.Shutdown(ctx) })

	// Start admin server
	var lv slog.LevelVar
	lv.Set(slog.LevelInfo)
	adminHandler := admin.New(mgr, cbStore, testStore, enc, adminToken, &lv)
	adminMux := http.NewServeMux()
	adminHandler.Register(adminMux)

	adminListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen admin: %v", err)
	}
	adminServer := &http.Server{Handler: adminMux}
	go adminServer.Serve(adminListener)
	t.Cleanup(func() { adminServer.Shutdown(ctx) })

	client := s3.New(s3.Options{
		BaseEndpoint: aws.String("http://" + proxyListener.Addr().String()),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		UsePathStyle: true,
	})

	return &encryptionTestEnv{
		proxyClient: client,
		adminAddr:   adminListener.Addr().String(),
		encryptor:   enc,
		rawStore:    testStore,
		manager:     mgr,
	}
}

// callAdmin makes a POST request to the admin API.
func (env *encryptionTestEnv) callAdmin(t *testing.T, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+env.adminAddr+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Admin-Token", adminToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin POST %s: status %d, body: %s", path, resp.StatusCode, body)
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return result
}

// queryEncryptionState returns the encrypted flag and size_bytes for an object.
func queryEncryptionState(t *testing.T, objectKey string) (encrypted bool, sizeBytes int64, plaintextSize *int64) {
	t.Helper()
	var ptSize sql.NullInt64
	err := testDB.QueryRow(
		"SELECT encrypted, size_bytes, plaintext_size FROM object_locations WHERE object_key = $1 LIMIT 1",
		objectKey,
	).Scan(&encrypted, &sizeBytes, &ptSize)
	if err != nil {
		t.Fatalf("queryEncryptionState(%q): %v", objectKey, err)
	}
	if ptSize.Valid {
		v := ptSize.Int64
		plaintextSize = &v
	}
	return
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

func TestEncryptDecryptExisting_RoundTrip(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put some plaintext objects through a non-encrypted proxy (use the global one)
	keys := make([]string, 3)
	bodies := make([][]byte, 3)
	for i := range keys {
		keys[i] = uniqueKey(t, "enc")
		bodies[i] = bytes.Repeat([]byte{byte('A' + i)}, 100+i*50)

		_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(keys[i]),
			Body:          bytes.NewReader(bodies[i]),
			ContentLength: aws.Int64(int64(len(bodies[i]))),
		})
		if err != nil {
			t.Fatalf("PutObject[%d]: %v", i, err)
		}
	}

	// Verify objects are unencrypted in DB
	for i, key := range keys {
		enc, sizeBytes, ptSize := queryEncryptionState(t, internalKey(key))
		if enc {
			t.Errorf("object[%d] should be unencrypted before encrypt-existing", i)
		}
		if sizeBytes != int64(len(bodies[i])) {
			t.Errorf("object[%d] size_bytes = %d, want %d", i, sizeBytes, len(bodies[i]))
		}
		if ptSize != nil {
			t.Errorf("object[%d] plaintext_size should be nil, got %d", i, *ptSize)
		}
	}

	// ---------------------------------------------------------------
	// ENCRYPT EXISTING
	// ---------------------------------------------------------------
	result := env.callAdmin(t, "/admin/api/encrypt-existing")
	if status, _ := result["status"].(string); status != "complete" {
		t.Fatalf("encrypt-existing status = %q, want complete", status)
	}
	encrypted := int(result["encrypted"].(float64))
	if encrypted != 3 {
		t.Errorf("encrypt-existing encrypted = %d, want 3", encrypted)
	}

	// Verify objects are encrypted in DB
	for i, key := range keys {
		enc, sizeBytes, ptSize := queryEncryptionState(t, internalKey(key))
		if !enc {
			t.Errorf("object[%d] should be encrypted after encrypt-existing", i)
		}
		// Ciphertext is larger than plaintext
		if sizeBytes <= int64(len(bodies[i])) {
			t.Errorf("object[%d] encrypted size_bytes = %d, should be > plaintext %d", i, sizeBytes, len(bodies[i]))
		}
		if ptSize == nil || *ptSize != int64(len(bodies[i])) {
			t.Errorf("object[%d] plaintext_size should be %d", i, len(bodies[i]))
		}
	}

	// Verify objects are still readable through the encrypted proxy (transparent decrypt)
	for i, key := range keys {
		resp, err := env.proxyClient.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject[%d] after encrypt: %v", i, err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll[%d]: %v", i, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Errorf("object[%d] body mismatch after encrypt: got %d bytes, want %d", i, len(got), len(bodies[i]))
		}
	}

	// ---------------------------------------------------------------
	// DECRYPT EXISTING
	// ---------------------------------------------------------------
	result = env.callAdmin(t, "/admin/api/decrypt-existing")
	if status, _ := result["status"].(string); status != "complete" {
		t.Fatalf("decrypt-existing status = %q, want complete", status)
	}
	decrypted := int(result["decrypted"].(float64))
	if decrypted != 3 {
		t.Errorf("decrypt-existing decrypted = %d, want 3", decrypted)
	}

	// Verify objects are unencrypted again in DB
	for i, key := range keys {
		enc, sizeBytes, ptSize := queryEncryptionState(t, internalKey(key))
		if enc {
			t.Errorf("object[%d] should be unencrypted after decrypt-existing", i)
		}
		if sizeBytes != int64(len(bodies[i])) {
			t.Errorf("object[%d] size_bytes = %d after decrypt, want %d", i, sizeBytes, len(bodies[i]))
		}
		if ptSize != nil {
			t.Errorf("object[%d] plaintext_size should be nil after decrypt, got %d", i, *ptSize)
		}
	}

	// Verify objects are readable through the non-encrypted proxy
	for i, key := range keys {
		resp, err := newS3Client(t).GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(virtualBucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject[%d] after decrypt: %v", i, err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll[%d]: %v", i, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Errorf("object[%d] body mismatch after decrypt: got %d bytes, want %d", i, len(got), len(bodies[i]))
		}
	}
}

func TestEncryptDecryptExisting_Empty(t *testing.T) {
	env := setupEncryptionEnv(t)

	// Encrypt with no objects — should succeed with 0 encrypted
	result := env.callAdmin(t, "/admin/api/encrypt-existing")
	if encrypted := int(result["encrypted"].(float64)); encrypted != 0 {
		t.Errorf("encrypt-existing on empty DB: encrypted = %d, want 0", encrypted)
	}

	// Decrypt with no encrypted objects — should succeed with 0 decrypted
	result = env.callAdmin(t, "/admin/api/decrypt-existing")
	if decrypted := int(result["decrypted"].(float64)); decrypted != 0 {
		t.Errorf("decrypt-existing on empty DB: decrypted = %d, want 0", decrypted)
	}
}

func TestEncryptDecryptExisting_IdempotentEncrypt(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put one object
	key := uniqueKey(t, "enc-idempotent")
	body := []byte("idempotent test data")
	_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Encrypt once
	env.callAdmin(t, "/admin/api/encrypt-existing")

	// Encrypt again — already-encrypted objects should not be re-encrypted
	result := env.callAdmin(t, "/admin/api/encrypt-existing")
	if encrypted := int(result["encrypted"].(float64)); encrypted != 0 {
		t.Errorf("second encrypt-existing should process 0 objects, got %d", encrypted)
	}

	// Object should still be readable
	resp, err := env.proxyClient.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after double-encrypt: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Error("body mismatch after idempotent encrypt")
	}
}

func TestDecryptExisting_IdempotentDecrypt(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put, encrypt, decrypt
	key := uniqueKey(t, "dec-idempotent")
	body := []byte("decrypt idempotent test")
	_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	env.callAdmin(t, "/admin/api/encrypt-existing")
	env.callAdmin(t, "/admin/api/decrypt-existing")

	// Decrypt again — already-decrypted objects should not be re-processed
	result := env.callAdmin(t, "/admin/api/decrypt-existing")
	if decrypted := int(result["decrypted"].(float64)); decrypted != 0 {
		t.Errorf("second decrypt-existing should process 0 objects, got %d", decrypted)
	}

	// Object should still be readable through the non-encrypted proxy
	resp, err := newS3Client(t).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Error("body mismatch after idempotent decrypt")
	}
}

// TestEncryptDecryptExisting_DirectBackendVerification verifies that the actual
// bytes on the backend change during encrypt/decrypt — not just the DB metadata.
func TestEncryptDecryptExisting_DirectBackendVerification(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "enc-direct")
	body := []byte("verify backend bytes change")
	_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Find which backend stores it
	backendName := queryObjectBackend(t, key)
	backend := allBackends[backendName]

	// Read raw bytes from backend — should be plaintext
	rawResult, err := backend.GetObject(ctx, internalKey(key), "")
	if err != nil {
		t.Fatalf("direct GetObject (pre-encrypt): %v", err)
	}
	rawBytes, _ := io.ReadAll(rawResult.Body)
	rawResult.Body.Close()
	if !bytes.Equal(rawBytes, body) {
		t.Fatal("raw backend bytes should be plaintext before encrypt")
	}

	// Encrypt
	env.callAdmin(t, "/admin/api/encrypt-existing")

	// Read raw bytes from backend — should be ciphertext (different from plaintext)
	encResult, err := backend.GetObject(ctx, internalKey(key), "")
	if err != nil {
		t.Fatalf("direct GetObject (post-encrypt): %v", err)
	}
	encBytes, _ := io.ReadAll(encResult.Body)
	encResult.Body.Close()
	if bytes.Equal(encBytes, body) {
		t.Fatal("raw backend bytes should NOT be plaintext after encrypt")
	}
	if len(encBytes) <= len(body) {
		t.Fatalf("encrypted bytes (%d) should be larger than plaintext (%d)", len(encBytes), len(body))
	}

	// Decrypt
	env.callAdmin(t, "/admin/api/decrypt-existing")

	// Read raw bytes from backend — should be plaintext again
	decResult, err := backend.GetObject(ctx, internalKey(key), "")
	if err != nil {
		t.Fatalf("direct GetObject (post-decrypt): %v", err)
	}
	decBytes, _ := io.ReadAll(decResult.Body)
	decResult.Body.Close()
	if !bytes.Equal(decBytes, body) {
		t.Fatalf("raw backend bytes should be plaintext after decrypt: got %d bytes, want %d", len(decBytes), len(body))
	}
}
