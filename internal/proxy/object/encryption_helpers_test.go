// -------------------------------------------------------------------------------
// Encryption Helper Tests
//
// Author: Alex Freidah
//
// Verifies the read-path decryption of GetObject responses, whole and ranged,
// where plaintext sizes and ciphertext ranges have to stay aligned, and the
// counting encryptor the write-path tests use to see KeyProvider round-trips.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/config"
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

// WrapDEK delegates to the wrapped provider but optionally returns
// wrapErr without incrementing the call counter so the test can
// simulate a Vault transit failure without losing visibility into
// the cache hit/miss accounting.
func (c *countingKeyProvider) WrapDEK(ctx context.Context, dek []byte) ([]byte, string, error) {
	if c.wrapErr != nil {
		return nil, "", c.wrapErr
	}
	c.wraps.Add(1)
	return c.inner.WrapDEK(ctx, dek)
}

// UnwrapDEK delegates to the wrapped provider; the test does not
// inject failures here because the cache covers the wrap path only.
func (c *countingKeyProvider) UnwrapDEK(ctx context.Context, wrapped []byte, keyID string) ([]byte, error) {
	return c.inner.UnwrapDEK(ctx, wrapped, keyID)
}

// KeyID returns the inner provider's KeyID so the cache key the
// encryptor builds remains stable across test calls.
func (c *countingKeyProvider) KeyID() string { return c.inner.KeyID() }

// newCountingEncryptor returns an Encryptor backed by a counting key
// provider so callers can inspect WrapDEK invocation counts.
func newCountingEncryptor(t *testing.T) (*encryption.Encryptor, *countingKeyProvider) {
	t.Helper()
	inner, err := encryption.NewStaticKeyProvider(make([]byte, config.KeySize))
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

// newTestEncryptor constructs a new test encryptor.
func newTestEncryptor(t *testing.T) *encryption.Encryptor {
	t.Helper()
	p, err := encryption.NewStaticKeyProvider(make([]byte, config.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewEncryptor(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// TestDecryptResponse_FullRead verifies the decrypt response full read contract.
// Asserts that encrypt:.
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

// TestDecryptResponse_RangeRead verifies the decrypt response range read contract.
// Asserts that encrypt:.
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

// TestDecryptResponse_RangeDecryptError verifies the decrypt response range decrypt error contract.
// Asserts that encrypt:.
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

// TestDecryptResponse_DecryptError verifies the decrypt response decrypt error contract.
// Asserts that encrypt:.
func TestDecryptResponse_DecryptError(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	// Valid key data but garbage ciphertext  -  decrypt should fail
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

// TestDecryptResponse_BadKeyData verifies the decrypt response bad key data path by exercising io.NopCloser, bytes.NewReader, context.Background.
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
