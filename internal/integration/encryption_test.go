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

// TestDecryptExisting_BackendNotFound verifies that decrypt-existing gracefully
// skips objects whose backend no longer exists and reports them as failed.
func TestDecryptExisting_BackendNotFound(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put and encrypt a real object
	key := uniqueKey(t, "dec-no-backend")
	body := []byte("backend will disappear")
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

	// Create a fake backend in the quotas table to satisfy the FK, then
	// rewrite the object's backend_name to point at it. The manager doesn't
	// know about this backend, so GetBackend will fail.
	_, err = testDB.ExecContext(ctx,
		"INSERT INTO backend_quotas (backend_name, bytes_used, bytes_limit, updated_at) VALUES ('ghost-backend', 0, 0, NOW()) ON CONFLICT DO NOTHING")
	if err != nil {
		t.Fatalf("insert ghost backend: %v", err)
	}
	_, err = testDB.ExecContext(ctx,
		"UPDATE object_locations SET backend_name = 'ghost-backend' WHERE object_key = $1",
		internalKey(key))
	if err != nil {
		t.Fatalf("update backend_name: %v", err)
	}

	result := env.callAdmin(t, "/admin/api/decrypt-existing")
	if failedCount := int(result["failed"].(float64)); failedCount != 1 {
		t.Errorf("expected 1 failed, got %d", failedCount)
	}
}

// TestDecryptExisting_CorruptedKeyData verifies that decrypt-existing gracefully
// skips objects with corrupted encryption key metadata.
func TestDecryptExisting_CorruptedKeyData(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put and encrypt a real object
	key := uniqueKey(t, "dec-corrupt-key")
	body := []byte("key data will be corrupted")
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

	// Corrupt the encryption_key in the DB (too short to unpack)
	_, err = testDB.ExecContext(ctx,
		"UPDATE object_locations SET encryption_key = $1 WHERE object_key = $2",
		[]byte("short"), internalKey(key))
	if err != nil {
		t.Fatalf("corrupt encryption_key: %v", err)
	}

	result := env.callAdmin(t, "/admin/api/decrypt-existing")
	if failedCount := int(result["failed"].(float64)); failedCount != 1 {
		t.Errorf("expected 1 failed (corrupt key data), got %d", failedCount)
	}
}

// TestDecryptExisting_WrongKey verifies that decrypt-existing gracefully skips
// objects whose wrapped DEK cannot be unwrapped (wrong master key).
func TestDecryptExisting_WrongKey(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put and encrypt a real object
	key := uniqueKey(t, "dec-wrong-key")
	body := []byte("encrypted with one key, decrypt with another")
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

	// Replace the wrapped DEK with garbage (valid nonce prefix + junk DEK)
	// UnpackKeyData expects 12+ bytes: first 12 = nonce, rest = wrappedDEK
	fakeKeyData := bytes.Repeat([]byte("X"), 12+32) // valid length but wrong ciphertext
	_, err = testDB.ExecContext(ctx,
		"UPDATE object_locations SET encryption_key = $1 WHERE object_key = $2",
		fakeKeyData, internalKey(key))
	if err != nil {
		t.Fatalf("replace encryption_key: %v", err)
	}

	result := env.callAdmin(t, "/admin/api/decrypt-existing")
	if failedCount := int(result["failed"].(float64)); failedCount != 1 {
		t.Errorf("expected 1 failed (wrong key), got %d", failedCount)
	}
}

// TestDecryptExisting_MixedSuccessAndFailure verifies that decrypt-existing
// processes all objects even when some fail, reporting correct counts.
func TestDecryptExisting_MixedSuccessAndFailure(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put two objects and encrypt them
	goodKey := uniqueKey(t, "dec-mixed-good")
	badKey := uniqueKey(t, "dec-mixed-bad")
	body := []byte("mixed test payload")

	for _, k := range []string{goodKey, badKey} {
		_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(virtualBucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			t.Fatalf("PutObject(%s): %v", k, err)
		}
	}
	env.callAdmin(t, "/admin/api/encrypt-existing")

	// Corrupt one object's key data
	_, err := testDB.ExecContext(ctx,
		"UPDATE object_locations SET encryption_key = $1 WHERE object_key = $2",
		[]byte("bad"), internalKey(badKey))
	if err != nil {
		t.Fatalf("corrupt key: %v", err)
	}

	result := env.callAdmin(t, "/admin/api/decrypt-existing")
	decrypted := int(result["decrypted"].(float64))
	failed := int(result["failed"].(float64))
	total := int(result["total"].(float64))

	if decrypted != 1 {
		t.Errorf("decrypted = %d, want 1", decrypted)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}

	// Good object should now be plaintext and readable
	resp, err := newS3Client(t).GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(virtualBucket),
		Key:    aws.String(goodKey),
	})
	if err != nil {
		t.Fatalf("GetObject good key: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, body) {
		t.Error("good object body mismatch after partial decrypt")
	}
}

// TestDecryptExisting_DownloadFails verifies that decrypt-existing gracefully
// skips objects that exist in the DB but have been deleted from the backend.
func TestDecryptExisting_DownloadFails(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	// Put and encrypt
	key := uniqueKey(t, "dec-download-fail")
	body := []byte("will be deleted from backend")
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

	// Delete the object directly from the backend (bypassing the orchestrator)
	backendName := queryObjectBackend(t, key)
	backend := allBackends[backendName]
	if err := backend.DeleteObject(ctx, internalKey(key)); err != nil {
		t.Fatalf("direct DeleteObject: %v", err)
	}

	// decrypt-existing should fail for this object (download returns 404)
	result := env.callAdmin(t, "/admin/api/decrypt-existing")
	if failedCount := int(result["failed"].(float64)); failedCount != 1 {
		t.Errorf("expected 1 failed (download error), got %d", failedCount)
	}
}

// TestEncryptExisting_DownloadFails verifies that encrypt-existing gracefully
// skips objects that exist in the DB but have been deleted from the backend.
func TestEncryptExisting_DownloadFails(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "enc-download-fail")
	body := []byte("will be deleted from backend")
	_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Delete the object directly from the backend
	backendName := queryObjectBackend(t, key)
	backend := allBackends[backendName]
	if err := backend.DeleteObject(ctx, internalKey(key)); err != nil {
		t.Fatalf("direct DeleteObject: %v", err)
	}

	result := env.callAdmin(t, "/admin/api/encrypt-existing")
	if failedCount := int(result["failed"].(float64)); failedCount != 1 {
		t.Errorf("expected 1 failed (download error), got %d", failedCount)
	}
}

// TestEncryptExisting_BackendNotFound verifies that encrypt-existing gracefully
// skips objects whose backend no longer exists.
func TestEncryptExisting_BackendNotFound(t *testing.T) {
	env := setupEncryptionEnv(t)
	ctx := context.Background()

	key := uniqueKey(t, "enc-no-backend")
	body := []byte("backend will disappear")
	_, err := newS3Client(t).PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(virtualBucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Create a fake backend in the quotas table to satisfy the FK, then
	// rewrite the object's backend_name to point at it.
	_, err = testDB.ExecContext(ctx,
		"INSERT INTO backend_quotas (backend_name, bytes_used, bytes_limit, updated_at) VALUES ('ghost-backend', 0, 0, NOW()) ON CONFLICT DO NOTHING")
	if err != nil {
		t.Fatalf("insert ghost backend: %v", err)
	}
	_, err = testDB.ExecContext(ctx,
		"UPDATE object_locations SET backend_name = 'ghost-backend' WHERE object_key = $1",
		internalKey(key))
	if err != nil {
		t.Fatalf("update backend_name: %v", err)
	}

	result := env.callAdmin(t, "/admin/api/encrypt-existing")
	if failedCount := int(result["failed"].(float64)); failedCount != 1 {
		t.Errorf("expected 1 failed, got %d", failedCount)
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
