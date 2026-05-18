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
	"context"
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
	er, err := newEncryptReader(bytes.NewReader(nil), dek, 1024, newChunkBuffers(1024), nil)
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
	er, err := newEncryptReader(bytes.NewReader(plaintext), dek, chunkSize, newChunkBuffers(chunkSize), nil)
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
	dr, err := newDecryptReader(hdrReader, dek, baseNonce, cs, 0, newChunkBuffers(cs), nil)
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

	er, err := newEncryptReader(bytes.NewReader(make([]byte, inputSize)), dek, chunkSize, newChunkBuffers(chunkSize), nil)
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
	dr, err := newDecryptReader(bytes.NewReader(short), dek, make([]byte, NonceSize), 1024, 0, newChunkBuffers(1024), nil)
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
	er, err := newEncryptReader(bytes.NewReader(make([]byte, chunkSize)), dek, chunkSize, newChunkBuffers(chunkSize), nil)
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
	dr, err := newDecryptReader(r, dek, baseNonce, cs, 99, newChunkBuffers(cs), nil) // wrong start chunk
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
		er, err := newEncryptReader(bytes.NewReader(data), dek, 64*1024, newChunkBuffers(64*1024), nil)
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
	er, err := newEncryptReader(bytes.NewReader(data), dek, chunkSize, newChunkBuffers(chunkSize), nil)
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
		dr, err := newDecryptReader(r, dek, baseNonce, cs, 0, newChunkBuffers(cs), nil)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, dr)
	}
}

// TestEncryptDecryptReaders_ZeroAllocsOnChunkHotPath pins the per-chunk
// allocation count for the streaming encrypt + decrypt readers. The
// constructor allocates a small constant number of buffers (cipher,
// gcm, baseNonce, header, plainBuf, nonceBuf, chunkOutBuf for encrypt
// and the equivalent set for decrypt). Once those are paid, gcm.Seal
// and gcm.Open run into pre-allocated dst buffers and the chunk loop
// itself must allocate nothing. A regression that re-introduces
// Seal(nil, ...) / Open(nil, ...) or per-chunk make() would show up
// as a multi-allocation-per-chunk number on a multi-chunk object.
//
// This test is the contract behind #885. The upper bounds below are
// generous (~2x current real numbers) so the test is not flaky
// against future constructor changes; the shape asserted is that
// allocs per whole-object encrypt do not grow with chunk count.
func TestEncryptDecryptReaders_ZeroAllocsOnChunkHotPath(t *testing.T) {
	// AllocsPerRun panics under t.Parallel(); intentionally serial.
	const chunkSize = 64 * 1024
	const objectSize = 1 << 20 // 1 MiB -> 16 chunks
	const allocsUpperBound = 15

	dek := testDEK()
	plaintext := make([]byte, objectSize)
	scratch := make([]byte, 16*1024)

	// One encrypt to capture the output we feed to the decrypt path.
	body, cs, baseNonce := encryptAllForBench(t, plaintext, dek, chunkSize)

	encOpen := func() io.Reader {
		er, err := newEncryptReader(bytes.NewReader(plaintext), dek, chunkSize, newChunkBuffers(chunkSize), nil)
		if err != nil {
			t.Fatalf("newEncryptReader: %v", err)
		}
		return er
	}
	decOpen := func() io.Reader {
		dr, err := newDecryptReader(bytes.NewReader(body), dek, baseNonce, cs, 0, newChunkBuffers(cs), nil)
		if err != nil {
			t.Fatalf("newDecryptReader: %v", err)
		}
		return dr
	}

	assertAllocsBelow(t, "encrypt", encOpen, scratch, allocsUpperBound)
	assertAllocsBelow(t, "decrypt", decOpen, scratch, allocsUpperBound)
}

// encryptAllForBench encrypts plaintext once with the given DEK/chunk
// size and returns the chunk body (after stripping the header) plus
// the parsed header fields. Helper extracted to keep
// TestEncryptDecryptReaders_ZeroAllocsOnChunkHotPath simple enough
// for Sonarqube's cognitive-complexity rule (go:S3776).
func encryptAllForBench(t *testing.T, plaintext, dek []byte, chunkSize int) (body []byte, cs int, baseNonce []byte) {
	t.Helper()
	er, err := newEncryptReader(bytes.NewReader(plaintext), dek, chunkSize, newChunkBuffers(chunkSize), nil)
	if err != nil {
		t.Fatalf("newEncryptReader: %v", err)
	}
	full, err := io.ReadAll(er)
	if err != nil {
		t.Fatalf("read encrypted: %v", err)
	}
	cs, baseNonce, err = ParseHeader(bytes.NewReader(full[:HeaderSize]))
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}
	return full[HeaderSize:], cs, baseNonce
}

// assertAllocsBelow drives one direction (encrypt or decrypt) through
// testing.AllocsPerRun and fails the test if allocations per whole-
// object run exceed the upper bound. Pre-#885 a 16-chunk encrypt
// allocated ~39 times and a decrypt ~22; the post-fix readers allocate
// in the single digits, so the bound is set generously to ~2x current.
func assertAllocsBelow(t *testing.T, label string, openReader func() io.Reader, scratch []byte, upperBound int) {
	t.Helper()
	allocs := testing.AllocsPerRun(50, func() {
		drainReader(t, openReader(), scratch)
	})
	if allocs > float64(upperBound) {
		t.Errorf("%s allocs/op = %.0f, want <= %d (regression: per-chunk allocation reintroduced?)", label, allocs, upperBound)
	}
}

// drainReader reads from r into scratch until io.EOF, failing the test
// on any other error. Extracted from the alloc assertion loop to keep
// each individual helper under the cognitive-complexity budget.
func drainReader(t *testing.T, r io.Reader, scratch []byte) {
	t.Helper()
	for {
		_, err := r.Read(scratch)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

// TestEncryptReader_ReleaseFiresExactlyOnce verifies the pool-return
// contract for the encrypt reader: the release closure must fire on
// io.EOF and must not fire again on any subsequent Read. A double
// release would push the same chunkBuffers into the pool twice and
// hand it to two concurrent streams.
func TestEncryptReader_ReleaseFiresExactlyOnce(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	dek := testDEK()
	plaintext := make([]byte, chunkSize*3+17) // multi-chunk + tail
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}

	var releases int
	er, err := newEncryptReader(bytes.NewReader(plaintext), dek, chunkSize, newChunkBuffers(chunkSize), func() {
		releases++
	})
	if err != nil {
		t.Fatalf("newEncryptReader: %v", err)
	}

	if _, err := io.Copy(io.Discard, er); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if releases != 1 {
		t.Fatalf("release count after drain = %d, want 1", releases)
	}

	// Extra Read after EOF must be a no-op for the release counter.
	buf := make([]byte, 16)
	if _, err := er.Read(buf); err != io.EOF {
		t.Fatalf("post-EOF Read err = %v, want io.EOF", err)
	}
	if releases != 1 {
		t.Fatalf("release count after post-EOF Read = %d, want 1", releases)
	}
}

// TestDecryptReader_ReleaseFiresExactlyOnce is the decrypt-side twin
// of the above. Same correctness story: pool integrity depends on
// release firing exactly once per stream.
func TestDecryptReader_ReleaseFiresExactlyOnce(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	dek := testDEK()
	plaintext := make([]byte, chunkSize*3+17)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand: %v", err)
	}

	er, err := newEncryptReader(bytes.NewReader(plaintext), dek, chunkSize, newChunkBuffers(chunkSize), nil)
	if err != nil {
		t.Fatalf("newEncryptReader: %v", err)
	}
	full, err := io.ReadAll(er)
	if err != nil {
		t.Fatalf("read encrypted: %v", err)
	}
	cs, baseNonce, err := ParseHeader(bytes.NewReader(full[:HeaderSize]))
	if err != nil {
		t.Fatalf("parse header: %v", err)
	}

	var releases int
	dr, err := newDecryptReader(bytes.NewReader(full[HeaderSize:]), dek, baseNonce, cs, 0, newChunkBuffers(cs), func() {
		releases++
	})
	if err != nil {
		t.Fatalf("newDecryptReader: %v", err)
	}

	if _, err := io.Copy(io.Discard, dr); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if releases != 1 {
		t.Fatalf("release count after drain = %d, want 1", releases)
	}

	buf := make([]byte, 16)
	if _, err := dr.Read(buf); err != io.EOF {
		t.Fatalf("post-EOF Read err = %v, want io.EOF", err)
	}
	if releases != 1 {
		t.Fatalf("release count after post-EOF Read = %d, want 1", releases)
	}
}

// TestEncryptor_PoolReuse_NoCrossStreamLeak verifies that running two
// distinct encrypt+decrypt round-trips back-to-back through the same
// Encryptor (which shares one sync.Pool) does not leak plaintext from
// the first stream into the second via a stale framed/plain buffer.
// A bug here would only surface when the second plaintext is shorter
// than the first - the tail of the reused buffer would still hold
// the first stream's data.
func TestEncryptor_PoolReuse_NoCrossStreamLeak(t *testing.T) {
	t.Parallel()
	const chunkSize = 64
	enc, err := NewEncryptor(testKeyProvider(t), chunkSize)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	ctx := context.Background()

	// First stream: long. Fills framed/plain buffers to near capacity.
	pt1 := bytes.Repeat([]byte{0xAA}, chunkSize*4+5)
	// Second stream: short. Reuses the same pooled buffers, so any
	// stale tail from pt1 would surface as corruption in pt2's decrypt
	// output (or in its ciphertext if framed wasn't truncated).
	pt2 := bytes.Repeat([]byte{0x55}, chunkSize/2)

	for i, pt := range [][]byte{pt1, pt2} {
		r, err := enc.Encrypt(ctx, bytes.NewReader(pt), int64(len(pt)))
		if err != nil {
			t.Fatalf("stream %d: Encrypt: %v", i, err)
		}
		ct, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("stream %d: read ct: %v", i, err)
		}
		dr, err := enc.Decrypt(ctx, bytes.NewReader(ct), r.WrappedDEK, r.KeyID)
		if err != nil {
			t.Fatalf("stream %d: Decrypt: %v", i, err)
		}
		got, err := io.ReadAll(dr)
		if err != nil {
			t.Fatalf("stream %d: read pt: %v", i, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("stream %d: plaintext mismatch (len got=%d want=%d) - pool leak suspected", i, len(got), len(pt))
		}
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
		er, _ := newEncryptReader(bytes.NewReader(data), dek, chunkSize, newChunkBuffers(chunkSize), nil)
		ct, _ := io.ReadAll(er)

		r := bytes.NewReader(ct)
		cs, baseNonce, _ := ParseHeader(r)
		dr, _ := newDecryptReader(r, dek, baseNonce, cs, 0, newChunkBuffers(cs), nil)
		_, _ = io.Copy(io.Discard, dr)
	}
}
