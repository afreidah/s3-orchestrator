// -------------------------------------------------------------------------------
// Range Translation Tests
//
// Author: Alex Freidah
//
// Tests for plaintext-to-ciphertext range translation including aligned chunks,
// cross-boundary ranges, single-byte ranges, invalid inputs, and recovering
// the chunk size an envelope was written with from its sizes.
// -------------------------------------------------------------------------------

package encryption

import (
	"testing"
)

// rangeOf translates a range of a 256-byte envelope written in 64-byte
// chunks, the object every test below reads.
func rangeOf(start, end int64) (*RangeResult, error) {
	return CiphertextRange(start, end, 256, ciphertextSizeExact(256, 64))
}

// TestChunkSizeOf_RecoversWriteSize pins that an envelope's chunk size is
// recovered from its plaintext and ciphertext sizes alone, and that sizes no
// layout produces are refused.
func TestChunkSizeOf_RecoversWriteSize(t *testing.T) {
	t.Parallel()
	for _, cs := range []int{64, 4096, 65536, 1 << 20} {
		for _, p := range []int64{int64(cs) + 1, 3 * int64(cs), 10*int64(cs) - 7} {
			if got, err := chunkSizeOf(p, ciphertextSizeExact(p, cs)); err != nil || got != cs {
				t.Errorf("chunkSizeOf(%d bytes in %d-byte chunks) = %d, %v", p, cs, got, err)
			}
		}
	}
	// One chunk reads the same under any size that holds it.
	if got, err := chunkSizeOf(100, ciphertextSizeExact(100, 65536)); err != nil || got < 100 {
		t.Errorf("single chunk: got %d, %v; want a size of at least 100", got, err)
	}
	for _, sizes := range [][2]int64{{0, HeaderSize}, {100, 100}, {100, ciphertextSizeExact(100, 64) + 1}} {
		if _, err := chunkSizeOf(sizes[0], sizes[1]); err == nil {
			t.Errorf("chunkSizeOf(%d, %d) succeeded, want an error", sizes[0], sizes[1])
		}
	}
}

// TestCiphertextRange_FirstChunk verifies the ciphertext range first chunk contract.
// Asserts that CiphertextRange:.
func TestCiphertextRange_FirstChunk(t *testing.T) {
	t.Parallel()
	r, err := rangeOf(0, 63)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 0 {
		t.Errorf("StartChunk = %d, want 0", r.StartChunk)
	}
	if r.SliceStart != 0 {
		t.Errorf("SliceStart = %d, want 0", r.SliceStart)
	}
	if r.SliceLen != 64 {
		t.Errorf("SliceLen = %d, want 64", r.SliceLen)
	}
	if r.BackendRange == "" {
		t.Error("BackendRange should not be empty")
	}
}

// TestCiphertextRange_CrossBoundary verifies the ciphertext range cross boundary contract.
// Asserts that CiphertextRange:.
func TestCiphertextRange_CrossBoundary(t *testing.T) {
	t.Parallel()
	// bytes 60-70 crosses chunk 0 (0-63) and chunk 1 (64-127)
	r, err := rangeOf(60, 70)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 0 {
		t.Errorf("StartChunk = %d, want 0", r.StartChunk)
	}
	if r.SliceStart != 60 {
		t.Errorf("SliceStart = %d, want 60", r.SliceStart)
	}
	if r.SliceLen != 11 {
		t.Errorf("SliceLen = %d, want 11", r.SliceLen)
	}
}

// TestCiphertextRange_SecondChunk verifies the ciphertext range second chunk contract.
// Asserts that CiphertextRange:.
func TestCiphertextRange_SecondChunk(t *testing.T) {
	t.Parallel()
	// bytes 64-127 -> chunk 1 only
	r, err := rangeOf(64, 127)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 1 {
		t.Errorf("StartChunk = %d, want 1", r.StartChunk)
	}
	if r.SliceStart != 0 {
		t.Errorf("SliceStart = %d, want 0", r.SliceStart)
	}
	if r.SliceLen != 64 {
		t.Errorf("SliceLen = %d, want 64", r.SliceLen)
	}
}

// TestCiphertextRange_SingleByte verifies the ciphertext range single byte contract.
// Asserts that CiphertextRange:.
func TestCiphertextRange_SingleByte(t *testing.T) {
	t.Parallel()
	r, err := rangeOf(100, 100)
	if err != nil {
		t.Fatalf("CiphertextRange: %v", err)
	}
	if r.StartChunk != 1 {
		t.Errorf("StartChunk = %d, want 1", r.StartChunk)
	}
	if r.SliceStart != 36 { // 100 % 64 = 36
		t.Errorf("SliceStart = %d, want 36", r.SliceStart)
	}
	if r.SliceLen != 1 {
		t.Errorf("SliceLen = %d, want 1", r.SliceLen)
	}
}

// TestCiphertextRange_InvalidRange verifies the ciphertext range invalid range behaviour described by the test name.
func TestCiphertextRange_InvalidRange(t *testing.T) {
	t.Parallel()
	_, err := rangeOf(10, 5)
	if err == nil {
		t.Error("expected error for end < start")
	}

	_, err = rangeOf(-1, 5)
	if err == nil {
		t.Error("expected error for negative start")
	}
}
