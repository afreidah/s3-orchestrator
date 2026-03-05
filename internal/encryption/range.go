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

import "fmt"

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

// RangeResult holds the translated ciphertext range and the slice offsets
// needed to extract the requested plaintext bytes after decryption.
type RangeResult struct {
	// BackendRange is the HTTP Range header value for the backend request
	// (e.g., "bytes=32-65595"). Empty string means fetch the entire object.
	BackendRange string

	// StartChunk is the zero-based index of the first chunk to decrypt.
	StartChunk uint64

	// SliceStart is the byte offset within the first decrypted chunk where
	// the requested plaintext range begins.
	SliceStart int64

	// SliceLen is the number of plaintext bytes to return from the
	// decrypted output.
	SliceLen int64
}

// CiphertextRange translates a plaintext byte range into the corresponding
// ciphertext range. The start and end parameters are inclusive byte offsets
// into the plaintext (matching HTTP Range semantics).
func CiphertextRange(start, end int64, chunkSize int) (*RangeResult, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid range: %d-%d", start, end)
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
		BackendRange: fmt.Sprintf("bytes=%d-%d", ctStart, ctEnd),
		StartChunk:   uint64(startChunk),
		SliceStart:   sliceStart,
		SliceLen:     sliceLen,
	}, nil
}
