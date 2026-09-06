// -------------------------------------------------------------------------------
// Encryption Tests - Envelope Encryption Round-Trip
//
// Author: Alex Freidah
//
// Tests for the Encryptor covering encrypt/decrypt round-trips at various
// sizes, range-based decryption across chunk boundaries, ciphertext size
// calculations, and key data packing.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// testKeyProvider is a simple ConfigKeyProvider for testing. Uses a fixed key.
func testKeyProvider(t *testing.T) *ConfigKeyProvider {
	t.Helper()
	// 32-byte key base64-encoded
	p, err := NewConfigKeyProvider("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", "test-0")
	if err != nil {
		t.Fatalf("NewConfigKeyProvider: %v", err)
	}
	return p
}

// testEncryptor builds an Encryptor with a deterministic master key
// and a 64-byte chunk size suitable for unit tests. The chunk size
// is small enough that round-trip tests exercise multiple chunks
// even with short plaintext.
func testEncryptor(t *testing.T, chunkSize int) *Encryptor {
	t.Helper()
	enc, err := NewEncryptor(testKeyProvider(t), chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// -------------------------------------------------------------------------
// ENCRYPT + DECRYPT ROUND-TRIP
// -------------------------------------------------------------------------

// TestEncryptDecrypt_Empty verifies the encrypt decrypt empty contract.
// Asserts that Encrypt:.
func TestEncryptDecrypt_Empty(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()

	result, err := enc.Encrypt(ctx, bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll ciphertext: %v", err)
	}

	if int64(len(ciphertext)) != result.CiphertextSize {
		t.Errorf("ciphertext len = %d, want %d", len(ciphertext), result.CiphertextSize)
	}

	// Decrypt should produce empty
	plain, err := enc.Decrypt(ctx, bytes.NewReader(ciphertext), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	got, err := io.ReadAll(plain)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("decrypted %d bytes, want 0", len(got))
	}
}

// TestEncryptDecrypt_OneByte verifies the encrypt decrypt one byte contract.
// Asserts that Encrypt:.
func TestEncryptDecrypt_OneByte(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()
	original := []byte{0x42}

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), 1)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll ciphertext: %v", err)
	}

	plain, err := enc.Decrypt(ctx, bytes.NewReader(ciphertext), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	got, err := io.ReadAll(plain)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}

	if !bytes.Equal(got, original) {
		t.Errorf("decrypted = %v, want %v", got, original)
	}
}

// TestEncryptDecrypt_ExactlyOneChunk verifies the encrypt decrypt exactly one chunk contract.
// Asserts that Encrypt:.
func TestEncryptDecrypt_ExactlyOneChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 128
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()

	original := make([]byte, chunkSize)
	_, _ = rand.Read(original)

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(chunkSize))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll ciphertext: %v", err)
	}

	plain, err := enc.Decrypt(ctx, bytes.NewReader(ciphertext), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	got, err := io.ReadAll(plain)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}

	if !bytes.Equal(got, original) {
		t.Error("decrypted data does not match original")
	}
}

// TestEncryptDecrypt_MultiChunk verifies the encrypt decrypt multi chunk contract.
// Asserts that Encrypt:.
func TestEncryptDecrypt_MultiChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()

	// 5.5 chunks worth of data
	original := make([]byte, chunkSize*5+chunkSize/2)
	_, _ = rand.Read(original)

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll ciphertext: %v", err)
	}

	if int64(len(ciphertext)) != result.CiphertextSize {
		t.Errorf("ciphertext len = %d, want %d", len(ciphertext), result.CiphertextSize)
	}

	plain, err := enc.Decrypt(ctx, bytes.NewReader(ciphertext), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	got, err := io.ReadAll(plain)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}

	if !bytes.Equal(got, original) {
		t.Error("decrypted data does not match original")
	}
}

// TestEncryptDecrypt_LargePayload verifies the encrypt decrypt large payload contract.
// Asserts that Encrypt:.
func TestEncryptDecrypt_LargePayload(t *testing.T) {
	t.Parallel()
	const chunkSize = 256
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()

	// ~100KB with non-aligned size
	original := make([]byte, 100*1024+37)
	_, _ = rand.Read(original)

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll ciphertext: %v", err)
	}

	plain, err := enc.Decrypt(ctx, bytes.NewReader(ciphertext), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	got, err := io.ReadAll(plain)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}

	if !bytes.Equal(got, original) {
		t.Errorf("decrypted data does not match original (len got=%d, want=%d)", len(got), len(original))
	}
}

// -------------------------------------------------------------------------
// RANGE DECRYPTION
// -------------------------------------------------------------------------

// TestDecryptRange_FirstChunk verifies the decrypt range first chunk contract.
// Asserts that Encrypt:.
func TestDecryptRange_FirstChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()

	original := make([]byte, chunkSize*4)
	for i := range original {
		original[i] = byte(i % 256)
	}

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Request bytes 10-30 (within first chunk)
	rng, err := CiphertextRange(10, 30, chunkSize)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}

	// Extract the ciphertext range (skip header, get the chunk bytes)
	ctBytes := ciphertext[rng.StartChunk*uint64(chunkSize+ChunkOverhead)+uint64(HeaderSize):]

	reader, n, err := enc.DecryptRange(ctx, bytes.NewReader(ctBytes), result.WrappedDEK, result.KeyID, rng, result.BaseNonce)
	if err != nil {
		t.Fatalf("DecryptRange: %v", err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if int64(len(got)) != n {
		t.Errorf("got %d bytes, DecryptRange reported %d", len(got), n)
	}

	want := original[10:31]
	if !bytes.Equal(got, want) {
		t.Errorf("range mismatch: got %v, want %v", got, want)
	}
}

// TestDecryptStored_FullRoundTrip verifies DecryptStored decrypts a full object
// from its packed key blob and echoes the supplied plaintext size.
func TestDecryptStored_FullRoundTrip(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()
	original := []byte("the stored key blob carries both the nonce and the wrapped DEK")

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	packed := PackKeyData(result.BaseNonce, result.WrappedDEK)
	reader, n, err := enc.DecryptStored(ctx, bytes.NewReader(ciphertext), packed, result.KeyID, int64(len(original)), nil)
	if err != nil {
		t.Fatalf("DecryptStored: %v", err)
	}
	if n != int64(len(original)) {
		t.Errorf("length = %d, want %d", n, len(original))
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("decrypted = %q, want %q", got, original)
	}
}

// TestDecryptStored_Range verifies DecryptStored routes to the range path and
// returns the requested plaintext slice and its length when rng is non-nil.
func TestDecryptStored_Range(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()
	original := make([]byte, chunkSize*4)
	for i := range original {
		original[i] = byte(i % 256)
	}

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	rng, err := CiphertextRange(10, 30, chunkSize)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	ctBytes := ciphertext[rng.StartChunk*uint64(chunkSize+ChunkOverhead)+uint64(HeaderSize):]

	packed := PackKeyData(result.BaseNonce, result.WrappedDEK)
	reader, n, err := enc.DecryptStored(ctx, bytes.NewReader(ctBytes), packed, result.KeyID, int64(len(original)), rng)
	if err != nil {
		t.Fatalf("DecryptStored: %v", err)
	}
	if n != rng.SliceLen {
		t.Errorf("length = %d, want SliceLen %d", n, rng.SliceLen)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll plaintext: %v", err)
	}
	if want := original[10:31]; !bytes.Equal(got, want) {
		t.Errorf("range mismatch: got %v, want %v", got, want)
	}
}

// TestDecryptStored_InvalidKeyData verifies a malformed packed key blob is
// reported as ErrInvalidKeyData before any decryption is attempted.
func TestDecryptStored_InvalidKeyData(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)

	reader, n, err := enc.DecryptStored(context.Background(), bytes.NewReader(nil), []byte("short"), "test-0", 0, nil)
	if !errors.Is(err, ErrInvalidKeyData) {
		t.Fatalf("err = %v, want ErrInvalidKeyData", err)
	}
	if reader != nil || n != 0 {
		t.Errorf("got reader=%v n=%d, want nil/0 on error", reader, n)
	}
}

// TestDecryptStored_Telemetry verifies DecryptStored owns the decrypt counters:
// a full success bumps ops[decrypt], a range success bumps ops[decrypt_range],
// and a malformed key blob bumps errors[decrypt,unpack_failed]. Not parallel so
// the before/after deltas are not perturbed by the parallel DecryptStored tests
// (which run in a later phase and are the only other callers of these counters).
func TestDecryptStored_Telemetry(t *testing.T) {
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()
	original := make([]byte, chunkSize*3)
	for i := range original {
		original[i] = byte(i)
	}

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	packed := PackKeyData(result.BaseNonce, result.WrappedDEK)

	opsFull := telemetry.EncryptionOpsTotal.WithLabelValues("decrypt")
	opsRange := telemetry.EncryptionOpsTotal.WithLabelValues("decrypt_range")
	errUnpack := telemetry.EncryptionErrorsTotal.WithLabelValues("decrypt", "unpack_failed")
	beforeFull := promtest.ToFloat64(opsFull)
	beforeRange := promtest.ToFloat64(opsRange)
	beforeErr := promtest.ToFloat64(errUnpack)

	if _, _, err := enc.DecryptStored(ctx, bytes.NewReader(ciphertext), packed, result.KeyID, int64(len(original)), nil); err != nil {
		t.Fatalf("DecryptStored full: %v", err)
	}
	rng, err := CiphertextRange(0, 10, chunkSize)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	ctBytes := ciphertext[uint64(HeaderSize):]
	if _, _, err := enc.DecryptStored(ctx, bytes.NewReader(ctBytes), packed, result.KeyID, int64(len(original)), rng); err != nil {
		t.Fatalf("DecryptStored range: %v", err)
	}
	if _, _, err := enc.DecryptStored(ctx, bytes.NewReader(nil), []byte("short"), result.KeyID, 0, nil); !errors.Is(err, ErrInvalidKeyData) {
		t.Fatalf("DecryptStored bad key: err = %v, want ErrInvalidKeyData", err)
	}

	if got := promtest.ToFloat64(opsFull) - beforeFull; got != 1 {
		t.Errorf("ops[decrypt] delta = %v, want 1", got)
	}
	if got := promtest.ToFloat64(opsRange) - beforeRange; got != 1 {
		t.Errorf("ops[decrypt_range] delta = %v, want 1", got)
	}
	if got := promtest.ToFloat64(errUnpack) - beforeErr; got != 1 {
		t.Errorf("errors[decrypt,unpack_failed] delta = %v, want 1", got)
	}
}

// TestDecryptRange_CrossChunkBoundary verifies the decrypt range cross chunk boundary contract.
// Asserts that Encrypt:.
func TestDecryptRange_CrossChunkBoundary(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()

	original := make([]byte, chunkSize*4)
	for i := range original {
		original[i] = byte(i % 256)
	}

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ciphertext, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Request bytes 60-70 (crosses chunk 0->1 boundary at byte 64)
	rng, err := CiphertextRange(60, 70, chunkSize)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}

	// Parse the backend range to extract ciphertext slice
	var ctStart, ctEnd int64
	_, _ = fmt.Sscanf(rng.BackendRange, "bytes=%d-%d", &ctStart, &ctEnd)
	ctSlice := ciphertext[ctStart : ctEnd+1]

	reader, n, err := enc.DecryptRange(ctx, bytes.NewReader(ctSlice), result.WrappedDEK, result.KeyID, rng, result.BaseNonce)
	if err != nil {
		t.Fatalf("DecryptRange: %v", err)
	}

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if int64(len(got)) != n {
		t.Errorf("got %d bytes, DecryptRange reported %d", len(got), n)
	}

	want := original[60:71]
	if !bytes.Equal(got, want) {
		t.Errorf("cross-chunk range mismatch: got %v, want %v", got, want)
	}
}

// -------------------------------------------------------------------------
// CIPHERTEXT SIZE
// -------------------------------------------------------------------------

// TestCiphertextSize_Zero verifies the ciphertext size zero contract.
// Asserts that CiphertextSize(0) = , want.
func TestCiphertextSize_Zero(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	got := enc.CiphertextSize(0)
	if got != int64(HeaderSize) {
		t.Errorf("CiphertextSize(0) = %d, want %d", got, HeaderSize)
	}
}

// TestCiphertextSize_OneChunk verifies the ciphertext size one chunk contract.
// Asserts that CiphertextSize(1) = , want.
func TestCiphertextSize_OneChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	// 1 byte -> 1 chunk
	got := enc.CiphertextSize(1)
	want := int64(HeaderSize + ChunkOverhead + 1)
	if got != want {
		t.Errorf("CiphertextSize(1) = %d, want %d", got, want)
	}
}

// TestCiphertextSize_ExactChunk verifies the ciphertext size exact chunk contract.
// Asserts that CiphertextSize() = , want.
func TestCiphertextSize_ExactChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	got := enc.CiphertextSize(int64(chunkSize))
	want := int64(HeaderSize + ChunkOverhead + chunkSize)
	if got != want {
		t.Errorf("CiphertextSize(%d) = %d, want %d", chunkSize, got, want)
	}
}

// TestCiphertextSize_MultiChunk verifies the ciphertext size multi chunk contract.
// Asserts that CiphertextSize() = , want.
func TestCiphertextSize_MultiChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	// 2.5 chunks
	plaintextSize := int64(chunkSize*2 + chunkSize/2)
	got := enc.CiphertextSize(plaintextSize)
	want := int64(HeaderSize) + 2*int64(ChunkOverhead+chunkSize) + int64(ChunkOverhead+chunkSize/2)
	if got != want {
		t.Errorf("CiphertextSize(%d) = %d, want %d", plaintextSize, got, want)
	}
}

// TestCiphertextSize_MatchesActualOutput verifies the ciphertext size matches actual output contract.
// Asserts that Encrypt():.
func TestCiphertextSize_MatchesActualOutput(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	ctx := context.Background()

	sizes := []int64{0, 1, 63, 64, 65, 128, 200, 1024}
	for _, sz := range sizes {
		data := make([]byte, sz)
		_, _ = rand.Read(data)

		result, err := enc.Encrypt(ctx, bytes.NewReader(data), sz)
		if err != nil {
			t.Fatalf("Encrypt(%d): %v", sz, err)
		}

		ct, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatalf("ReadAll(%d): %v", sz, err)
		}

		predicted := enc.CiphertextSize(sz)
		if int64(len(ct)) != predicted {
			t.Errorf("size %d: actual ciphertext = %d, predicted = %d", sz, len(ct), predicted)
		}
	}
}

// -------------------------------------------------------------------------
// PACK / UNPACK KEY DATA
// -------------------------------------------------------------------------

// TestPackUnpackKeyData_RoundTrip verifies the pack unpack key data round trip contract.
// Asserts that UnpackKeyData:.
func TestPackUnpackKeyData_RoundTrip(t *testing.T) {
	t.Parallel()
	baseNonce := make([]byte, NonceSize)
	_, _ = rand.Read(baseNonce)

	wrappedDEK := make([]byte, 60) // realistic wrapped DEK size
	_, _ = rand.Read(wrappedDEK)

	packed := PackKeyData(baseNonce, wrappedDEK)

	gotNonce, gotDEK, err := UnpackKeyData(packed)
	if err != nil {
		t.Fatalf("UnpackKeyData: %v", err)
	}

	if !bytes.Equal(gotNonce, baseNonce) {
		t.Errorf("nonce mismatch")
	}
	if !bytes.Equal(gotDEK, wrappedDEK) {
		t.Errorf("wrappedDEK mismatch")
	}
}

// TestUnpackKeyData_TooShort verifies the unpack key data too short behaviour described by the test name.
func TestUnpackKeyData_TooShort(t *testing.T) {
	t.Parallel()
	_, _, err := UnpackKeyData(make([]byte, NonceSize))
	if err == nil {
		t.Error("expected error for data <= NonceSize")
	}
}

// -------------------------------------------------------------------------
// HEADER PARSING
// -------------------------------------------------------------------------

// TestParseHeader_Valid verifies the parse header valid contract.
// Asserts that Encrypt:.
func TestParseHeader_Valid(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 256)
	ctx := context.Background()

	data := make([]byte, 10)
	_, _ = rand.Read(data)

	result, err := enc.Encrypt(ctx, bytes.NewReader(data), 10)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	ct, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	chunkSize, baseNonce, err := ParseHeader(bytes.NewReader(ct))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}

	if chunkSize != 256 {
		t.Errorf("chunkSize = %d, want 256", chunkSize)
	}
	if !bytes.Equal(baseNonce, result.BaseNonce) {
		t.Error("baseNonce mismatch")
	}
}

// TestParseHeader_InvalidMagic verifies the parse header invalid magic path by exercising bytes.NewReader.
func TestParseHeader_InvalidMagic(t *testing.T) {
	t.Parallel()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], "XXXX")
	_, _, err := ParseHeader(bytes.NewReader(hdr))
	if err == nil {
		t.Error("expected error for invalid magic")
	}
}

// TestParseHeader_UnsupportedVersion verifies the parse header unsupported version path by exercising bytes.NewReader.
func TestParseHeader_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], headerMagic[:])
	hdr[4] = 0x99
	_, _, err := ParseHeader(bytes.NewReader(hdr))
	if err == nil {
		t.Error("expected error for unsupported version")
	}
}

// TestParseHeader_TooShort verifies the parse header too short path by exercising bytes.NewReader.
func TestParseHeader_TooShort(t *testing.T) {
	t.Parallel()
	_, _, err := ParseHeader(bytes.NewReader(make([]byte, 10)))
	if err == nil {
		t.Error("expected error for truncated header")
	}
}

// -------------------------------------------------------------------------
// ACCESSOR METHODS
// -------------------------------------------------------------------------

// TestEncryptor_ChunkSize verifies the encryptor chunk size contract.
// Asserts that ChunkSize = , want 4096.
func TestEncryptor_ChunkSize(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 4096)
	if enc.ChunkSize() != 4096 {
		t.Errorf("ChunkSize = %d, want 4096", enc.ChunkSize())
	}
}

// TestNewEncryptor_InvalidChunkSize verifies the new encryptor invalid chunk size contract.
// Asserts that NewEncryptor(chunkSize=) should return error.
func TestNewEncryptor_InvalidChunkSize(t *testing.T) {
	t.Parallel()
	for _, cs := range []int{0, -1, -100} {
		_, err := NewEncryptor(testKeyProvider(t), cs)
		if err == nil {
			t.Errorf("NewEncryptor(chunkSize=%d) should return error", cs)
		}
	}
}

// TestEncryptor_Provider verifies the encryptor provider path by exercising enc.Provider.
func TestEncryptor_Provider(t *testing.T) {
	t.Parallel()
	p := testKeyProvider(t)
	enc, err := NewEncryptor(p, 64)
	if err != nil {
		t.Fatal(err)
	}
	if enc.Provider() != p {
		t.Error("Provider() should return the same provider")
	}
}

// -------------------------------------------------------------------------
// DECRYPT ERROR PATHS
// -------------------------------------------------------------------------

// TestDecrypt_BadWrappedDEK verifies the decrypt bad wrapped dek contract.
// Asserts that Encrypt:.
func TestDecrypt_BadWrappedDEK(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()

	// Encrypt something valid first
	original := []byte("test data")
	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct, _ := io.ReadAll(result.Body)

	// Try to decrypt with corrupted wrapped DEK
	_, err = enc.Decrypt(ctx, bytes.NewReader(ct), []byte("garbage"), result.KeyID)
	if err == nil {
		t.Error("expected error for corrupted wrapped DEK")
	}
}

// TestDecrypt_CorruptedCiphertext verifies the decrypt corrupted ciphertext contract.
// Asserts that Encrypt:.
func TestDecrypt_CorruptedCiphertext(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()

	original := []byte("test data for corruption")
	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct, _ := io.ReadAll(result.Body)

	// Corrupt a byte in the ciphertext (after header)
	ct[HeaderSize+NonceSize+5] ^= 0xFF

	reader, err := enc.Decrypt(ctx, bytes.NewReader(ct), result.WrappedDEK, result.KeyID)
	if err != nil {
		// Error during setup is fine too
		return
	}

	// Should fail during read (auth tag mismatch)
	_, err = io.ReadAll(reader)
	if err == nil {
		t.Error("expected error for corrupted ciphertext")
	}
}

// TestDecryptRange_BadWrappedDEK verifies the decrypt range bad wrapped dek path by exercising context.Background, enc.DecryptRange, bytes.NewReader.
func TestDecryptRange_BadWrappedDEK(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()

	rng, _ := CiphertextRange(0, 10, 64)
	_, _, err := enc.DecryptRange(ctx, bytes.NewReader(nil), []byte("garbage"), "test-0", rng, make([]byte, NonceSize))
	if err == nil {
		t.Error("expected error for bad wrapped DEK")
	}
}

// -------------------------------------------------------------------------
// SMALL-READ BUFFER SIZES (exercises buffering paths)
// -------------------------------------------------------------------------

// TestEncryptDecrypt_SmallReads exercises the buffer-drain paths in
// Encrypt and Decrypt by consuming both readers one byte at a time. The
// resulting plaintext must match the original input.
func TestEncryptDecrypt_SmallReads(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()

	original := make([]byte, 200)
	_, _ = rand.Read(original)

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct := readAllOneByteAtATime(t, result.Body)

	reader, err := enc.Decrypt(ctx, bytes.NewReader(ct), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	plain := readAllOneByteAtATime(t, reader)

	if !bytes.Equal(plain, original) {
		t.Error("small-read round-trip mismatch")
	}
}

// readAllOneByteAtATime drains r into a byte slice using single-byte
// Reads. Used to deliberately exercise the encryptor's buffering paths
// when the consumer never reads a full chunk in one call.
func readAllOneByteAtATime(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
}

// -------------------------------------------------------------------------
// ENCRYPT WITH DEK (FAILOVER REUSE)
// -------------------------------------------------------------------------

// TestEncryptWithDEK_RoundTrip verifies the encrypt with dek round trip contract.
// Asserts that Encrypt:.
func TestEncryptWithDEK_RoundTrip(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()
	original := make([]byte, 200)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}

	dek, wrappedDEK, keyID, err := enc.GenerateAndWrapDEK(ctx)
	if err != nil {
		t.Fatalf("GenerateAndWrapDEK: %v", err)
	}

	// First encrypt  -  the part that pays for the wrap
	first, err := enc.EncryptWithDEK(bytes.NewReader(original), int64(len(original)), dek, wrappedDEK, keyID)
	if err != nil {
		t.Fatalf("EncryptWithDEK first: %v", err)
	}
	ct1, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("ReadAll first: %v", err)
	}

	// Second encrypt  -  a later part under the same upload-level DEK
	second, err := enc.EncryptWithDEK(bytes.NewReader(original), int64(len(original)), dek, wrappedDEK, keyID)
	if err != nil {
		t.Fatalf("EncryptWithDEK second: %v", err)
	}
	ct2, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatalf("ReadAll second: %v", err)
	}

	// Ciphertexts must differ (different nonces)
	if bytes.Equal(ct1, ct2) {
		t.Error("ciphertexts should differ due to different nonces")
	}

	// Both must decrypt to same plaintext
	for i, ct := range [][]byte{ct1, ct2} {
		res := []*EncryptResult{first, second}[i]
		plain, err := enc.Decrypt(ctx, bytes.NewReader(ct), res.WrappedDEK, res.KeyID)
		if err != nil {
			t.Fatalf("Decrypt[%d]: %v", i, err)
		}
		got, err := io.ReadAll(plain)
		if err != nil {
			t.Fatalf("ReadAll[%d]: %v", i, err)
		}
		if !bytes.Equal(got, original) {
			t.Errorf("Decrypt[%d] mismatch", i)
		}
	}
}

// TestEncryptWithDEK_DifferentNonce verifies the encrypt with dek different nonce path by exercising context.Background, enc.Encrypt, bytes.NewReader.
func TestEncryptWithDEK_DifferentNonce(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()
	data := []byte("test data for nonce check")

	dek, wrappedDEK, keyID, err := enc.GenerateAndWrapDEK(ctx)
	if err != nil {
		t.Fatal(err)
	}

	first, err := enc.EncryptWithDEK(bytes.NewReader(data), int64(len(data)), dek, wrappedDEK, keyID)
	if err != nil {
		t.Fatal(err)
	}
	// Drain body to finalize
	if _, err := io.ReadAll(first.Body); err != nil {
		t.Fatal(err)
	}

	second, err := enc.EncryptWithDEK(bytes.NewReader(data), int64(len(data)), dek, wrappedDEK, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(second.Body); err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(first.BaseNonce, second.BaseNonce) {
		t.Error("BaseNonce should differ between calls (random per encrypt)")
	}
}

// failingKeyProvider always returns an error from WrapDEK so tests
// can drive the wrap-error branch in GenerateAndWrapDEK.
type failingKeyProvider struct{}

func (failingKeyProvider) WrapDEK(_ context.Context, _ []byte) ([]byte, string, error) {
	return nil, "", errors.New("simulated wrap failure")
}
func (failingKeyProvider) UnwrapDEK(_ context.Context, _ []byte, _ string) ([]byte, error) {
	return nil, errors.New("simulated unwrap failure")
}
func (failingKeyProvider) KeyID() string { return "fail-0" }

// TestGenerateAndWrapDEK_WrapError covers the branch in
// GenerateAndWrapDEK where the KeyProvider rejects the wrap call.
func TestGenerateAndWrapDEK_WrapError(t *testing.T) {
	t.Parallel()
	enc, err := NewEncryptor(failingKeyProvider{}, 64*1024)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	if _, _, _, err := enc.GenerateAndWrapDEK(context.Background()); err == nil {
		t.Fatal("expected wrap error, got nil")
	}
}

// TestGenerateAndWrapDEK_HappyPath covers the helper that
// CreateMultipartUpload uses to wrap a single shared DEK at the
// start of a multipart upload. Verifies the unwrapped DEK round-
// trips back to the same 32-byte material the helper produced.
func TestGenerateAndWrapDEK_HappyPath(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64*1024)
	dek, wrapped, keyID, err := enc.GenerateAndWrapDEK(context.Background())
	if err != nil {
		t.Fatalf("GenerateAndWrapDEK: %v", err)
	}
	if len(dek) != 32 {
		t.Errorf("dek length = %d, want 32", len(dek))
	}
	if len(wrapped) == 0 {
		t.Error("wrapped DEK is empty")
	}
	if keyID == "" {
		t.Error("keyID is empty")
	}
	unwrapped, err := enc.Provider().UnwrapDEK(context.Background(), wrapped, keyID)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(unwrapped, dek) {
		t.Error("UnwrapDEK did not return the original DEK")
	}
}
