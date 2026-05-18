// -------------------------------------------------------------------------------
// Nonce Derivation Benchmarks - Per-Chunk XOR Throughput
//
// Author: Alex Freidah
//
// Measures the cost of deriveNonce, which is called once per chunk during
// encrypt and decrypt. For a 1 GB object at 256 KB chunk size, this runs
// ~4000 times per operation. The benchmark isolates the XOR derivation from
// AES-GCM overhead.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"crypto/rand"
	"io"
	"sync"
	"testing"
)

// BenchmarkDeriveNonce measures the cost of a single nonce derivation (copy
// base nonce + XOR chunk index into last 8 bytes).
func BenchmarkDeriveNonce(b *testing.B) {
	base := make([]byte, NonceSize)
	_, _ = rand.Read(base)
	dst := make([]byte, NonceSize)

	for b.Loop() {
		deriveNonce(dst, base, 42)
	}
}

// BenchmarkDeriveNonce_Sequential simulates the sequential nonce derivation
// pattern used during streaming encrypt/decrypt of a multi-chunk object.
func BenchmarkDeriveNonce_Sequential(b *testing.B) {
	base := make([]byte, NonceSize)
	_, _ = rand.Read(base)
	dst := make([]byte, NonceSize)

	for b.Loop() {
		for idx := range uint64(1000) {
			deriveNonce(dst, base, idx)
		}
	}
}

// BenchmarkChunkNonce measures chunkNonce which allocates a new nonce slice
// per call (used in some code paths that cannot reuse a buffer).
func BenchmarkChunkNonce(b *testing.B) {
	base := make([]byte, NonceSize)
	_, _ = rand.Read(base)

	for b.Loop() {
		_ = chunkNonce(base, 42)
	}
}

// drainAll reads r to EOF into a scratch buffer reused across iterations.
// Used by the streaming benchmarks below so the discard side does not
// contribute its own allocations to the measured numbers.
func drainAll(b *testing.B, r io.Reader, scratch []byte) {
	for {
		_, err := r.Read(scratch)
		if err == io.EOF {
			return
		}
		if err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkEncryptReader_StreamingAllocs exercises the streaming
// encryption hot path through a sync.Pool of chunkBuffers, matching
// production wiring on *Encryptor. PR #885 turned every chunk into
// allocation-free Seal-into-dst, then pooled the per-stream buffers
// so a pool-warm whole-object encrypt costs only the DEK + cipher
// setup, not the chunkSize buffer. Stays in the low single digits
// of allocs/op regardless of chunk count.
func BenchmarkEncryptReader_StreamingAllocs(b *testing.B) {
	const chunkSize = 64 * 1024
	const objectSize = 1 << 20 // 1 MiB -> 16 chunks

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		b.Fatalf("rand: %v", err)
	}
	plaintext := make([]byte, objectSize)
	if _, err := rand.Read(plaintext); err != nil {
		b.Fatalf("rand: %v", err)
	}
	src := bytes.NewReader(plaintext)
	scratch := make([]byte, 16*1024)

	pool := sync.Pool{New: func() any { return newChunkBuffers(chunkSize) }}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = src.Seek(0, io.SeekStart)
		bufs := pool.Get().(*chunkBuffers)
		release := func() { pool.Put(bufs) }
		er, err := newEncryptReader(src, dek, chunkSize, bufs, release)
		if err != nil {
			release()
			b.Fatalf("newEncryptReader: %v", err)
		}
		drainAll(b, er, scratch)
	}
}

// BenchmarkDecryptReader_StreamingAllocs is the decrypt-side twin of
// the above. Same pool-warm shape: Open-into-dst plus pooled
// chunkBuffers (PR #885).
func BenchmarkDecryptReader_StreamingAllocs(b *testing.B) {
	const chunkSize = 64 * 1024
	const objectSize = 1 << 20 // 1 MiB -> 16 chunks

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		b.Fatalf("rand: %v", err)
	}
	plaintext := make([]byte, objectSize)
	if _, err := rand.Read(plaintext); err != nil {
		b.Fatalf("rand: %v", err)
	}

	// Pre-encrypt once to feed the decrypt benchmark deterministic
	// input. The constructor's random base nonce only matters for
	// the encrypt direction; the decrypt reader just consumes
	// whatever it's handed.
	encSrc := bytes.NewReader(plaintext)
	er, err := newEncryptReader(encSrc, dek, chunkSize, newChunkBuffers(chunkSize), nil)
	if err != nil {
		b.Fatalf("newEncryptReader: %v", err)
	}
	full, err := io.ReadAll(er)
	if err != nil {
		b.Fatalf("read encrypted: %v", err)
	}
	if len(full) < HeaderSize {
		b.Fatalf("encrypted output shorter than header: %d", len(full))
	}
	hdr := full[:HeaderSize]
	body := full[HeaderSize:]
	chunkSizeOut, baseNonce, err := ParseHeader(bytes.NewReader(hdr))
	if err != nil {
		b.Fatalf("parse header: %v", err)
	}
	scratch := make([]byte, 16*1024)

	pool := sync.Pool{New: func() any { return newChunkBuffers(chunkSizeOut) }}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		bufs := pool.Get().(*chunkBuffers)
		release := func() { pool.Put(bufs) }
		dr, err := newDecryptReader(bytes.NewReader(body), dek, baseNonce, chunkSizeOut, 0, bufs, release)
		if err != nil {
			release()
			b.Fatalf("newDecryptReader: %v", err)
		}
		drainAll(b, dr, scratch)
	}
}
