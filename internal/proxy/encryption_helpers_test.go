// -------------------------------------------------------------------------------
// Encryption Helper Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// countingKeyProvider wraps a real KeyProvider and counts WrapDEK calls so
// tests can assert that the DEK cache short-circuits the KeyProvider on
// retries. wrapErr, when set, returns an error from WrapDEK without
// incrementing the counter, simulating a Vault failure.
type countingKeyProvider struct {
	inner   encryption.KeyProvider
	wraps   atomic.Int32
	wrapErr error
}

func (c *countingKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	if c.wrapErr != nil {
		return nil, "", c.wrapErr
	}
	c.wraps.Add(1)
	return c.inner.WrapDEK(ctx, dek)
}

func (c *countingKeyProvider) UnwrapDEK(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	return c.inner.UnwrapDEK(ctx, wrapped, keyID)
}

func (c *countingKeyProvider) KeyID() string { return c.inner.KeyID() }

// newCountingEncryptor returns an Encryptor backed by a counting key
// provider so callers can inspect WrapDEK invocation counts.
func newCountingEncryptor(t *testing.T) (*encryption.Encryptor, *countingKeyProvider) {
	t.Helper()
	inner, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatal(err)
	}
	cp := &countingKeyProvider{inner: inner}
	enc, err := encryption.NewEncryptor(cp, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc, cp
}

func newTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	p, err := encryption.NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewEncryptor(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func TestEncryptBody_Success(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	plain := []byte("hello world encryption test data")

	body, ciphertextSize, meta, err := encryptBody(context.Background(), enc, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("encryptBody: %v", err)
	}
	if body == nil {
		t.Fatal("expected non-nil body")
	}
	if ciphertextSize <= int64(len(plain)) {
		t.Errorf("ciphertext size %d should be larger than plaintext %d", ciphertextSize, len(plain))
	}
	if meta == nil {
		t.Fatal("expected non-nil encryption meta")
	}
	if !meta.Encrypted {
		t.Error("expected Encrypted=true")
	}
	if meta.PlaintextSize != int64(len(plain)) {
		t.Errorf("PlaintextSize = %d, want %d", meta.PlaintextSize, len(plain))
	}
	if meta.KeyID == "" {
		t.Error("expected non-empty KeyID")
	}
	if len(meta.EncryptionKey) == 0 {
		t.Error("expected non-empty EncryptionKey")
	}

	// Consume the body to verify it's readable
	ciphertext, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read encrypted body: %v", err)
	}
	if int64(len(ciphertext)) != ciphertextSize {
		t.Errorf("read %d bytes, expected %d", len(ciphertext), ciphertextSize)
	}
}

func TestDecryptResponse_FullRead(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	plain := []byte("hello world decryption test data")

	// Encrypt first
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(encResult.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: int64(len(plain)),
	}

	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(ciphertext)),
		Size: int64(len(ciphertext)),
	}

	err = decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err != nil {
		t.Fatalf("decryptResponse: %v", err)
	}

	decrypted, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Errorf("decrypted content mismatch: got %q, want %q", decrypted, plain)
	}
	if r.Size != int64(len(plain)) {
		t.Errorf("Size = %d, want %d", r.Size, len(plain))
	}
}

func TestEncryptBody_ThenDecryptResponse_RoundTrip(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	plain := []byte("round trip test with helpers - verifies encrypt and decrypt are compatible")

	// Encrypt via helper
	body, ciphertextSize, meta, err := encryptBody(context.Background(), enc, bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("encryptBody: %v", err)
	}
	ciphertext, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	// Decrypt via helper
	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: meta.EncryptionKey,
		KeyID:         meta.KeyID,
		PlaintextSize: meta.PlaintextSize,
	}
	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(ciphertext)),
		Size: ciphertextSize,
	}

	err = decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err != nil {
		t.Fatalf("decryptResponse: %v", err)
	}

	decrypted, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(decrypted, plain) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d", len(decrypted), len(plain))
	}
}

func TestDecryptResponse_RangeRead(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)
	// Build a plaintext larger than one chunk (chunk size = 64 bytes)
	plain := bytes.Repeat([]byte("A"), 256)

	// Encrypt the full object
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(encResult.Body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: int64(len(plain)),
	}

	// Request a range within the plaintext
	ptStart, ptEnd := int64(10), int64(50)
	rng, err := encryption.CiphertextRange(ptStart, ptEnd, enc.ChunkSize())
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}

	// Parse the backend range to extract the ciphertext slice
	var ctStart, ctEnd int64
	if _, err := fmt.Sscanf(rng.BackendRange, "bytes=%d-%d", &ctStart, &ctEnd); err != nil {
		t.Fatalf("parse BackendRange %q: %v", rng.BackendRange, err)
	}
	ctEnd++ // BackendRange end is inclusive, slice end is exclusive
	if ctEnd > int64(len(ciphertext)) {
		ctEnd = int64(len(ciphertext))
	}
	rangeData := ciphertext[ctStart:ctEnd]

	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader(rangeData)),
		Size: int64(len(rangeData)),
	}

	err = decryptResponse(context.Background(), enc, r, loc, rng, ptStart, ptEnd)
	if err != nil {
		t.Fatalf("decryptResponse range: %v", err)
	}

	decrypted, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read decrypted range: %v", err)
	}

	expected := plain[ptStart : ptEnd+1]
	if !bytes.Equal(decrypted, expected) {
		t.Errorf("range mismatch: got %d bytes, want %d", len(decrypted), len(expected))
	}
	if r.ContentRange == "" {
		t.Error("expected ContentRange to be set for range request")
	}
}

func TestDecryptResponse_RangeDecryptError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	// Valid key data but garbage ciphertext for range decrypt
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader(bytes.Repeat([]byte("B"), 256)), 256)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	_, _ = io.ReadAll(encResult.Body)

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: 256,
	}

	rng, _ := encryption.CiphertextRange(10, 50, enc.ChunkSize())

	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("garbage-ciphertext-for-range"))),
		Size: 28,
	}

	err = decryptResponse(context.Background(), enc, r, loc, rng, 10, 50)
	if err == nil {
		t.Fatal("expected error for corrupted range ciphertext")
	}
}

func TestDecryptResponse_DecryptError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	// Valid key data but garbage ciphertext — decrypt should fail
	encResult, err := enc.Encrypt(context.Background(), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	// Read and discard the real ciphertext
	_, _ = io.ReadAll(encResult.Body)

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: encryption.PackKeyData(encResult.BaseNonce, encResult.WrappedDEK),
		KeyID:         encResult.KeyID,
		PlaintextSize: 1,
	}

	// Feed garbage instead of real ciphertext
	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("not-real-ciphertext-data-at-all"))),
		Size: 31,
	}

	err = decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err == nil {
		t.Fatal("expected error for corrupted ciphertext")
	}
}

// -------------------------------------------------------------------------
// encryptForPut
// -------------------------------------------------------------------------

func TestEncryptForPut_FirstCallPopulatesStateAndMeta(t *testing.T) {
	t.Parallel()
	enc, _ := newCountingEncryptor(t)
	plain := []byte("first call payload")
	var state putEncryptState

	body, ctSize, meta, err := encryptForPut(context.Background(), enc, plain, int64(len(plain)), &state)
	if err != nil {
		t.Fatalf("encryptForPut: %v", err)
	}
	if state.dek == nil || state.wrappedDEK == nil || state.keyID == "" {
		t.Errorf("state not populated: dek=%v wrappedDEK=%v keyID=%q", state.dek, state.wrappedDEK, state.keyID)
	}
	if !meta.Encrypted {
		t.Error("meta.Encrypted = false, want true")
	}
	if meta.PlaintextSize != int64(len(plain)) {
		t.Errorf("meta.PlaintextSize = %d, want %d", meta.PlaintextSize, len(plain))
	}
	if meta.KeyID == "" {
		t.Error("meta.KeyID is empty")
	}
	if meta.ContentHash != "" {
		t.Errorf("meta.ContentHash = %q, want empty (caller layers integrity)", meta.ContentHash)
	}
	if ctSize <= int64(len(plain)) {
		t.Errorf("ciphertext size %d should exceed plaintext %d", ctSize, len(plain))
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("read body: %v", err)
	}
}

func TestEncryptForPut_RetryReusesCachedDEK(t *testing.T) {
	t.Parallel()
	enc, cp := newCountingEncryptor(t)
	plain := []byte("retry payload — wraps once across N calls")
	var state putEncryptState

	const calls = 3
	for i := range calls {
		body, _, _, err := encryptForPut(context.Background(), enc, plain, int64(len(plain)), &state)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if _, err := io.ReadAll(body); err != nil {
			t.Fatalf("call %d read: %v", i, err)
		}
	}
	if got := cp.wraps.Load(); got != 1 {
		t.Errorf("WrapDEK called %d times across %d encryptForPut calls, want 1", got, calls)
	}
}

func TestEncryptForPut_RetryUsesFreshNonce(t *testing.T) {
	t.Parallel()
	enc, _ := newCountingEncryptor(t)
	plain := []byte("nonce uniqueness check")
	var state putEncryptState

	_, _, meta1, err := encryptForPut(context.Background(), enc, plain, int64(len(plain)), &state)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, _, meta2, err := encryptForPut(context.Background(), enc, plain, int64(len(plain)), &state)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	nonce1, _, err := encryption.UnpackKeyData(meta1.EncryptionKey)
	if err != nil {
		t.Fatalf("unpack first: %v", err)
	}
	nonce2, _, err := encryption.UnpackKeyData(meta2.EncryptionKey)
	if err != nil {
		t.Fatalf("unpack second: %v", err)
	}
	if bytes.Equal(nonce1, nonce2) {
		t.Errorf("base nonces must differ across retries (cached DEK + same nonce = AES-GCM key reuse)")
	}
}

func TestEncryptForPut_RoundTripDecrypts(t *testing.T) {
	t.Parallel()
	enc, _ := newCountingEncryptor(t)
	plain := []byte("round-trip payload via encryptForPut")
	var state putEncryptState

	body, _, meta, err := encryptForPut(context.Background(), enc, plain, int64(len(plain)), &state)
	if err != nil {
		t.Fatalf("encryptForPut: %v", err)
	}
	ciphertext, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	_, wrappedDEK, err := encryption.UnpackKeyData(meta.EncryptionKey)
	if err != nil {
		t.Fatalf("unpack key data: %v", err)
	}
	plainReader, err := enc.Decrypt(context.Background(), bytes.NewReader(ciphertext), wrappedDEK, meta.KeyID)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	got, err := io.ReadAll(plainReader)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plain)
	}
}

func TestEncryptForPut_WrapErrorLeavesStateEmpty(t *testing.T) {
	t.Parallel()
	enc, cp := newCountingEncryptor(t)
	cp.wrapErr = errors.New("simulated KeyProvider failure")

	var state putEncryptState
	plain := []byte("doomed payload")
	_, _, _, err := encryptForPut(context.Background(), enc, plain, int64(len(plain)), &state)
	if err == nil {
		t.Fatal("expected error from failing KeyProvider")
	}
	if !errors.Is(err, cp.wrapErr) {
		// fmt.Errorf("encrypt: %w", err) wraps the underlying provider error.
		t.Errorf("error chain does not contain provider error: %v", err)
	}
	if state.dek != nil || state.wrappedDEK != nil || state.keyID != "" {
		t.Errorf("state must remain empty so a retry attempts to wrap again: %+v", state)
	}
}

func TestDecryptResponse_BadKeyData(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	loc := &core.ObjectLocation{
		Encrypted:     true,
		EncryptionKey: []byte("garbage"),
		KeyID:         "test-0",
	}
	r := &s3be.GetObjectResult{
		Body: io.NopCloser(bytes.NewReader([]byte("ciphertext"))),
		Size: 10,
	}

	err := decryptResponse(context.Background(), enc, r, loc, nil, 0, 0)
	if err == nil {
		t.Fatal("expected error for bad key data")
	}
}
