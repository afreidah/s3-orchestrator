// -------------------------------------------------------------------------------
// Range Translation - Plaintext to Ciphertext Offset Math
//
// Author: Alex Freidah
//
// Translates plaintext byte ranges into ciphertext byte ranges for encrypted
// objects. Allows range requests (HTTP Range header) to fetch only the
// necessary encrypted chunks from the backend, avoiding full-object reads.
// -------------------------------------------------------------------------------

package encryption

import (
	"fmt"
	"math/bits"

	"github.com/afreidah/s3-orchestrator/internal/util/byterange"
)

// ciphertextSizeExact computes the ciphertext size accounting for the last
// chunk being smaller than chunkSize.
func ciphertextSizeExact(plaintextSize int64, chunkSize int) int64 {
	if plaintextSize == 0 {
		return int64(HeaderSize)
	}
	cs := int64(chunkSize)
	fullChunks := plaintextSize / cs
	remainder := plaintextSize % cs

	total := int64(HeaderSize)
	total += fullChunks * (int64(ChunkOverhead) + cs)
	if remainder > 0 {
		total += int64(ChunkOverhead) + remainder
	}
	return total
}

// chunkSizeOf recovers the chunk size an envelope was written with from its
// plaintext and ciphertext sizes, which fix how many chunks it holds. Only one
// power of two gives two or more chunks that count; a single chunk reads the
// same under any size that holds it, and the smallest is returned.
func chunkSizeOf(plaintextSize, ciphertextSize int64) (int, error) {
	if chunks := (ciphertextSize - HeaderSize - plaintextSize) / ChunkOverhead; chunks > 0 && plaintextSize > 0 {
		perChunk := uint64((plaintextSize + chunks - 1) / chunks) //nolint:gosec // G115: both operands positive
		cs := 1 << bits.Len64(perChunk-1)
		if ciphertextSizeExact(plaintextSize, cs) == ciphertextSize {
			return cs, nil
		}
	}
	return 0, fmt.Errorf("no chunk size gives %d plaintext bytes a %d-byte envelope", plaintextSize, ciphertextSize)
}

// RangeResult holds the translated ciphertext range and the slice offsets
// needed to extract the requested plaintext bytes after decryption.
type RangeResult struct {
	BackendRange string // the Range header to send, e.g. "bytes=32-65595"; empty fetches the whole object
	ChunkSize    int    // the chunk size the envelope was written with
	StartChunk   uint64 // zero-based index of the first chunk to decrypt
	SliceStart   int64  // offset within that first decrypted chunk where the plaintext range begins
	SliceLen     int64  // plaintext bytes to return from the decrypted output
}

// CiphertextRange translates a plaintext byte range into the ciphertext range
// of an envelope holding plaintextSize bytes in ciphertextSize. The start and
// end parameters are inclusive byte offsets into the plaintext (matching HTTP
// Range semantics).
func CiphertextRange(start, end, plaintextSize, ciphertextSize int64) (*RangeResult, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid range: %d-%d", start, end)
	}
	chunkSize, err := chunkSizeOf(plaintextSize, ciphertextSize)
	if err != nil {
		return nil, err
	}

	cs := int64(chunkSize)
	co := int64(ChunkOverhead)

	startChunk := start / cs
	endChunk := end / cs

	// Ciphertext offset of the first needed chunk (after the header)
	ctStart := int64(HeaderSize) + startChunk*(cs+co)

	// Ciphertext offset of the end of the last needed chunk
	ctEnd := int64(HeaderSize) + (endChunk+1)*(cs+co) - 1

	// Plaintext offset within the first decrypted chunk
	sliceStart := start % cs

	// Total plaintext bytes to return
	sliceLen := end - start + 1

	return &RangeResult{
		BackendRange: byterange.Header(ctStart, ctEnd),
		ChunkSize:    chunkSize,
		StartChunk:   uint64(startChunk), //nolint:gosec // G115: startChunk derived from non-negative start offset
		SliceStart:   sliceStart,
		SliceLen:     sliceLen,
	}, nil
}
