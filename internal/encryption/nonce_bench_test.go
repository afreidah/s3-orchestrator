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
	"crypto/rand"
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
