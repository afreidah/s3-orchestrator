// -------------------------------------------------------------------------------
// Chunked Encryption Tests
//
// Author: Alex Freidah
//
// Tests for streaming chunk encryption/decryption: round-trip at various sizes,
// header parsing, nonce derivation, and edge cases.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
)

// testDEK returns a fixed 256-bit key for deterministic tests.
func testDEK() []byte {
	dek := make([]byte, 32)
	for i := range dek {
		dek[i] = byte(i)
	}
	return dek
}

// TestChunkNonce_UniquePerIndex verifies the chunk nonce unique per index path by exercising bytes.Equal.
func TestChunkNonce_UniquePerIndex(t *testing.T) {
	t.Parallel()
	base := make([]byte, NonceSize)
	for i := range base {
		base[i] = 0xff
	}

	n0 := chunkNonce(base, 0)
	n1 := chunkNonce(base, 1)
	n2 := chunkNonce(base, 2)

	if bytes.Equal(n0, n1) {
		t.Error("nonce 0 and 1 should differ")
	}
	if bytes.Equal(n1, n2) {
		t.Error("nonce 1 and 2 should differ")
	}
}

// TestChunkNonce_DoesNotMutateBase verifies the chunk nonce does not mutate base path by exercising bytes.Equal.
func TestChunkNonce_DoesNotMutateBase(t *testing.T) {
	t.Parallel()
	base := make([]byte, NonceSize)
	copy(base, "base-nonce!!")
	original := make([]byte, NonceSize)
	copy(original, base)

	_ = chunkNonce(base, 42)

	if !bytes.Equal(base, original) {
		t.Error("chunkNonce mutated the base nonce")
	}
}

// TestRoundTrip_EmptyInput verifies the round trip empty input contract.
// Asserts that ciphertext len = , want (header only).
func TestRoundTrip_EmptyInput(t *testing.T) {
	t.Parallel()
	dek := testDEK()
	er, err := newEncryptReader(bytes.NewReader(nil), dek, 1024)
	if err != nil {
		t.Fatal(err)
	}

	ct, err := io.ReadAll(er)
	if err != nil {
		t.Fatal(err)
	}

	// Should produce only a header, no chunks
	if len(ct) != HeaderSize {
		t.Fatalf("ciphertext len = %d, want %d (header only)", len(ct), HeaderSize)
	}
}

// TestRoundTrip_ExactChunkSize verifies the round trip exact chunk size behaviour described by the test name.
func TestRoundTrip_ExactChunkSize(t *testing.T) {
	t.Parallel()
	testRoundTrip(t, 1024, 1024)
}

// TestRoundTrip_OneByteOverChunk verifies the round trip one byte over chunk behaviour described by the test name.
func TestRoundTrip_OneByteOverChunk(t *testing.T) {
	t.Parallel()
	testRoundTrip(t, 1024, 1025)
}

// TestRoundTrip_OneByteUnderChunk verifies the round trip one byte under chunk behaviour described by the test name.
func TestRoundTrip_OneByteUnderChunk(t *testing.T) {
	t.Parallel()
	testRoundTrip(t, 1024, 1023)
}

// TestRoundTrip_SmallChunkLargeInput verifies the round trip small chunk large input behaviour described by the test name.
func TestRoundTrip_SmallChunkLargeInput(t *testing.T) {
	t.Parallel()
	testRoundTrip(t, 64, 1000)
}

// TestRoundTrip_SingleByte verifies the round trip single byte behaviour described by the test name.
func TestRoundTrip_SingleByte(t *testing.T) {
	t.Parallel()
	testRoundTrip(t, 1024, 1)
}

// TestRoundTrip_MultipleFullChunks verifies the round trip multiple full chunks behaviour described by the test name.
func TestRoundTrip_MultipleFullChunks(t *testing.T) {
	t.Parallel()
	testRoundTrip(t, 256, 256*5)
}

// testRoundTrip is the shared body of the encrypt/decrypt round-trip
// table tests. Encrypts the plaintext, decrypts the ciphertext, and
// asserts byte-for-byte equality plus the size invariants.
func testRoundTrip(t *testing.T, chunkSize, inputSize int) {
	t.Helper()

	plaintext := make([]byte, inputSize)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatal(err)
	}

	dek := testDEK()

	// Encrypt
	er, err := newEncryptReader(bytes.NewReader(plaintext), dek, chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := io.ReadAll(er)
	if err != nil {
		t.Fatal(err)
	}

	// Parse header
	hdrReader := bytes.NewReader(ct)
	cs, baseNonce, err := ParseHeader(hdrReader)
	if err != nil {
		t.Fatal(err)
	}
	if cs != chunkSize {
		t.Fatalf("header chunk size = %d, want %d", cs, chunkSize)
	}

	// Decrypt
	dr, err := newDecryptReader(hdrReader, dek, baseNonce, cs, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(dr)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, plaintext) {
		t.Errorf("decrypted len = %d, want %d", len(got), len(plaintext))
	}
}

// TestChunkParseHeader_InvalidMagic verifies the chunk parse header invalid magic path by exercising bytes.NewReader.
func TestChunkParseHeader_InvalidMagic(t *testing.T) {
	t.Parallel()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], "BAAD")
	_, _, err := ParseHeader(bytes.NewReader(hdr))
	if err == nil {
		t.Fatal("expected error for invalid magic")
	}
}

// TestChunkParseHeader_UnsupportedVersion verifies the chunk parse header unsupported version path by exercising bytes.NewReader.
func TestChunkParseHeader_UnsupportedVersion(t *testing.T) {
	t.Parallel()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], headerMagic[:])
	hdr[4] = 0x99
	_, _, err := ParseHeader(bytes.NewReader(hdr))
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
}

// TestChunkParseHeader_TooShort verifies the chunk parse header too short path by exercising bytes.NewReader.
func TestChunkParseHeader_TooShort(t *testing.T) {
	t.Parallel()
	_, _, err := ParseHeader(bytes.NewReader([]byte("short")))
	if err == nil {
		t.Fatal("expected error for truncated header")
	}
}

// TestChunkParseHeader_ValidRoundTrip verifies the chunk parse header valid round trip contract.
// Asserts that chunk size = , want 4096.
func TestChunkParseHeader_ValidRoundTrip(t *testing.T) {
	t.Parallel()
	hdr := make([]byte, HeaderSize)
	copy(hdr[0:4], headerMagic[:])
	hdr[4] = 0x01
	binary.BigEndian.PutUint32(hdr[5:9], 4096)
	copy(hdr[9:21], "base-nonce!!")

	cs, nonce, err := ParseHeader(bytes.NewReader(hdr))
	if err != nil {
		t.Fatal(err)
	}
	if cs != 4096 {
		t.Errorf("chunk size = %d, want 4096", cs)
	}
	if string(nonce) != "base-nonce!!" {
		t.Errorf("nonce = %q, want %q", nonce, "base-nonce!!")
	}
}

// TestCiphertextSize verifies the ciphertext size contract.
// Asserts that ciphertext len = , want.
func TestCiphertextSize(t *testing.T) {
	t.Parallel()
	dek := testDEK()
	chunkSize := 1024
	inputSize := 2500 // 3 chunks: 1024 + 1024 + 452

	er, err := newEncryptReader(bytes.NewReader(make([]byte, inputSize)), dek, chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := io.ReadAll(er)
	if err != nil {
		t.Fatal(err)
	}

	// Expected: header + 3 chunks * (nonce + ciphertext + tag)
	// Chunk 0: NonceSize + 1024 + TagSize
	// Chunk 1: NonceSize + 1024 + TagSize
	// Chunk 2: NonceSize + 452  + TagSize
	expected := HeaderSize + 3*(NonceSize+TagSize) + inputSize
	if len(ct) != expected {
		t.Errorf("ciphertext len = %d, want %d", len(ct), expected)
	}
}

// TestDecryptReader_ChunkTooShort verifies the decrypt reader chunk too short path by exercising bytes.NewReader, io.ReadAll.
func TestDecryptReader_ChunkTooShort(t *testing.T) {
	t.Parallel()
	dek := testDEK()
	// Feed a truncated chunk (just a few bytes, less than NonceSize+TagSize)
	short := make([]byte, 5)
	dr, err := newDecryptReader(bytes.NewReader(short), dek, make([]byte, NonceSize), 1024, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(dr)
	if err == nil {
		t.Fatal("expected error for chunk too short")
	}
}

// TestDecryptReader_NonceMismatch verifies the decrypt reader nonce mismatch path by exercising bytes.NewReader, io.ReadAll.
func TestDecryptReader_NonceMismatch(t *testing.T) {
	t.Parallel()
	dek := testDEK()
	chunkSize := 64

	// Encrypt a small payload
	er, err := newEncryptReader(bytes.NewReader(make([]byte, chunkSize)), dek, chunkSize)
	if err != nil {
		t.Fatal(err)
	}
	ct, err := io.ReadAll(er)
	if err != nil {
		t.Fatal(err)
	}

	// Parse header, then decrypt starting at wrong chunk index
	r := bytes.NewReader(ct)
	cs, baseNonce, err := ParseHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	dr, err := newDecryptReader(r, dek, baseNonce, cs, 99) // wrong start chunk
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(dr)
	if err == nil {
		t.Fatal("expected nonce mismatch error")
	}
}

// BenchmarkEncryptReader measures the encrypt reader path by exercising bytes.NewReader, io.Copy.
func BenchmarkEncryptReader(b *testing.B) {
	dek := testDEK()
	data := make([]byte, 1<<20) // 1 MiB

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		er, err := newEncryptReader(bytes.NewReader(data), dek, 64*1024)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, er)
	}
}

// BenchmarkDecryptReader measures the decrypt reader path by exercising bytes.NewReader, io.ReadAll, io.Copy.
func BenchmarkDecryptReader(b *testing.B) {
	dek := testDEK()
	chunkSize := 64 * 1024
	data := make([]byte, 1<<20) // 1 MiB

	// Pre-encrypt
	er, err := newEncryptReader(bytes.NewReader(data), dek, chunkSize)
	if err != nil {
		b.Fatal(err)
	}
	ct, err := io.ReadAll(er)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		r := bytes.NewReader(ct)
		cs, baseNonce, err := ParseHeader(r)
		if err != nil {
			b.Fatal(err)
		}
		dr, err := newDecryptReader(r, dek, baseNonce, cs, 0)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, dr)
	}
}

// BenchmarkRoundTrip measures the round trip path by exercising bytes.NewReader, io.ReadAll, io.Copy.
func BenchmarkRoundTrip(b *testing.B) {
	dek := testDEK()
	chunkSize := 64 * 1024
	data := make([]byte, 1<<20) // 1 MiB

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		er, _ := newEncryptReader(bytes.NewReader(data), dek, chunkSize)
		ct, _ := io.ReadAll(er)

		r := bytes.NewReader(ct)
		cs, baseNonce, _ := ParseHeader(r)
		dr, _ := newDecryptReader(r, dek, baseNonce, cs, 0)
		_, _ = io.Copy(io.Discard, dr)
	}
}
