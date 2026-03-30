// -------------------------------------------------------------------------------
// Encryption Tests - Envelope Encryption Round-Trip
//
// Author: Alex Freidah
//
// Tests for the Encryptor covering encrypt/decrypt round-trips at various
// sizes, range-based decryption across chunk boundaries, ciphertext size
// calculations, key data packing, and MD5 ETag generation.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // G501: MD5 used to verify S3 ETag computation
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
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

	// Request bytes 60-70 (crosses chunk 0→1 boundary at byte 64)
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

func TestCiphertextSize_Zero(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	got := enc.CiphertextSize(0)
	if got != int64(HeaderSize) {
		t.Errorf("CiphertextSize(0) = %d, want %d", got, HeaderSize)
	}
}

func TestCiphertextSize_OneChunk(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc := testEncryptor(t, chunkSize)
	// 1 byte → 1 chunk
	got := enc.CiphertextSize(1)
	want := int64(HeaderSize + ChunkOverhead + 1)
	if got != want {
		t.Errorf("CiphertextSize(1) = %d, want %d", got, want)
	}
}

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
// MD5 ETAG
// -------------------------------------------------------------------------

func TestEncrypt_PlaintextMD5(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()

	original := []byte("hello, world!")
	expectedMD5 := md5.Sum(original) //nolint:gosec // G401: MD5 used to verify S3 ETag computation
	expectedHex := hex.EncodeToString(expectedMD5[:])

	result, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Must read all ciphertext to finalize the MD5
	if _, err := io.ReadAll(result.Body); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if result.PlaintextMD5 != expectedHex {
		t.Errorf("PlaintextMD5 = %q, want %q", result.PlaintextMD5, expectedHex)
	}
}

// -------------------------------------------------------------------------
// PACK / UNPACK KEY DATA
// -------------------------------------------------------------------------

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

func TestParseHeader_InvalidMagic(t *testing.T) {
	t.Parallel()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], "XXXX")
	_, _, err := ParseHeader(bytes.NewReader(hdr))
	if err == nil {
		t.Error("expected error for invalid magic")
	}
}

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

func TestEncryptor_ChunkSize(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 4096)
	if enc.ChunkSize() != 4096 {
		t.Errorf("ChunkSize = %d, want 4096", enc.ChunkSize())
	}
}

func TestNewEncryptor_InvalidChunkSize(t *testing.T) {
	t.Parallel()
	for _, cs := range []int{0, -1, -100} {
		_, err := NewEncryptor(testKeyProvider(t), cs)
		if err == nil {
			t.Errorf("NewEncryptor(chunkSize=%d) should return error", cs)
		}
	}
}

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

	// Read ciphertext one byte at a time to exercise buffer drain paths
	var ct []byte
	buf := make([]byte, 1)
	for {
		n, err := result.Body.Read(buf)
		if n > 0 {
			ct = append(ct, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	// Decrypt with small reads too
	reader, err := enc.Decrypt(ctx, bytes.NewReader(ct), result.WrappedDEK, result.KeyID)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	var plain []byte
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			plain = append(plain, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	if !bytes.Equal(plain, original) {
		t.Error("small-read round-trip mismatch")
	}
}

// -------------------------------------------------------------------------
// ENCRYPT WITH DEK (FAILOVER REUSE)
// -------------------------------------------------------------------------

func TestEncryptWithDEK_RoundTrip(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()
	original := make([]byte, 200)
	if _, err := rand.Read(original); err != nil {
		t.Fatal(err)
	}

	// First encrypt — normal path (wraps DEK via provider)
	first, err := enc.Encrypt(ctx, bytes.NewReader(original), int64(len(original)))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct1, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("ReadAll first: %v", err)
	}
	if first.RawDEK == nil {
		t.Fatal("RawDEK should be set after Encrypt")
	}

	// Second encrypt — reuse DEK (simulates failover retry)
	second, err := enc.EncryptWithDEK(bytes.NewReader(original), int64(len(original)), first.RawDEK, first.WrappedDEK, first.KeyID)
	if err != nil {
		t.Fatalf("EncryptWithDEK: %v", err)
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

func TestEncryptWithDEK_DifferentNonce(t *testing.T) {
	t.Parallel()
	enc := testEncryptor(t, 64)
	ctx := context.Background()
	data := []byte("test data for nonce check")

	first, err := enc.Encrypt(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	// Drain body to finalize
	if _, err := io.ReadAll(first.Body); err != nil {
		t.Fatal(err)
	}

	second, err := enc.EncryptWithDEK(bytes.NewReader(data), int64(len(data)), first.RawDEK, first.WrappedDEK, first.KeyID)
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
