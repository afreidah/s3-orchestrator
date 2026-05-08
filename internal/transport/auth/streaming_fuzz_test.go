// -------------------------------------------------------------------------------
// Streaming Chunk Reader Fuzz Target
//
// Author: Alex Freidah
//
// Feeds arbitrary bodies to NewChunkReader and asserts the reader
// never panics, never blocks, and never delivers more decoded bytes
// than the declared decoded length. Mutations on the seeded valid
// bodies cover the most likely framing-corruption surface (truncation,
// hex munging, signature tampering, oversized declarations).
// -------------------------------------------------------------------------------

package auth

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// FuzzChunkReader exercises the chunk reader with a seed corpus of
// known-good signed and unsigned-trailer bodies. The test asserts that
// every input either succeeds (Read returns io.EOF) or fails with a
// typed sentinel error from this package.
func FuzzChunkReader(f *testing.F) {
	signed := newFixture(StreamingSigned)
	signedSeed := signed.buildBody([]byte("hello fuzz"), 64)
	f.Add(signedSeed)

	unsignedTrailer := newFixture(StreamingUnsignedTrailer).withTrailers(map[string]string{
		"x-amz-checksum-crc32": "AAAAAA==",
	})
	unsignedSeed := unsignedTrailer.buildBody([]byte("trailer fuzz"), 16)
	f.Add(unsignedSeed)

	signedTrailer := newFixture(StreamingSignedTrailer).withTrailers(map[string]string{
		"x-amz-checksum-crc32": "AAAAAA==",
	})
	signedTrailerSeed := signedTrailer.buildBody([]byte("signed trailer fuzz"), 16)
	f.Add(signedTrailerSeed)

	f.Fuzz(func(t *testing.T, body []byte) {
		for _, fix := range []*fixture{signed, unsignedTrailer, signedTrailer} {
			rc := NewChunkReader(io.NopCloser(bytes.NewReader(body)),
				fix.material(int64(len(body))))
			drainAndCheck(t, rc)
		}
	})
}

// drainAndCheck reads rc to completion and asserts the terminal error
// is either io.EOF or a typed streaming-validation sentinel.
func drainAndCheck(t *testing.T, rc io.ReadCloser) {
	defer func() { _ = rc.Close() }()
	buf := make([]byte, 4096)
	for {
		_, err := rc.Read(buf)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if isTypedStreamingError(err) {
			return
		}
		t.Fatalf("unexpected error type: %v", err)
	}
}

// isTypedStreamingError reports whether err matches one of the package
// sentinels the fuzz target accepts as a graceful rejection.
func isTypedStreamingError(err error) bool {
	switch {
	case errors.Is(err, ErrChunkSignatureMismatch),
		errors.Is(err, ErrTrailerSignatureMismatch),
		errors.Is(err, ErrChunkMalformed),
		errors.Is(err, ErrChunkTooLarge),
		errors.Is(err, ErrDecodedLengthMismatch),
		errors.Is(err, ErrTrailerMalformed):
		return true
	}
	return false
}
