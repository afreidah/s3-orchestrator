// -------------------------------------------------------------------------------
// Encryption Helper Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	s3be "github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/encryption"
	"github.com/afreidah/s3-orchestrator/internal/store"
)

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

	loc := &store.ObjectLocation{
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
	loc := &store.ObjectLocation{
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

	loc := &store.ObjectLocation{
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

	loc := &store.ObjectLocation{
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

	loc := &store.ObjectLocation{
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

func TestDecryptResponse_BadKeyData(t *testing.T) {
	t.Parallel()
	enc := newTestEncryptor(t)

	loc := &store.ObjectLocation{
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
