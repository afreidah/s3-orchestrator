// -------------------------------------------------------------------------------
// Chunk-Framing Detection Tests
//
// Author: Alex Freidah
//
// Table-driven unit tests for Detect. Covers signed and unsigned-trailer
// happy paths, common false-positive shapes (hex-prefixed text, truncated
// headers), and explicit boundary cases (zero-size chunk, oversized chunk).
// -------------------------------------------------------------------------------

package chunkframing

import (
	"fmt"
	"testing"
)

// signedFrame builds a leading line for a signed aws-chunked body of the
// given size. The chunk-signature is zero-padded so the regex sees the
// required 64-hex shape.
func signedFrame(size int) []byte {
	return fmt.Appendf(nil, "%x;chunk-signature=%064x\r\n", size, 0)
}

// unsignedFrame builds two consecutive unsigned chunk frames each carrying
// a body of the given size. The bodies are filled with a constant byte so
// the chain is structurally valid.
func unsignedFrame(size int) []byte {
	header := fmt.Appendf(nil, "%x\r\n", size)
	body := make([]byte, size)
	for i := range body {
		body[i] = 'a'
	}
	tail := []byte("\r\n")
	one := append(append(header, body...), tail...)
	return append(one, one...)
}

// TestDetect_HappyPaths covers the three accepted variants.
func TestDetect_HappyPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		head []byte
		want Variant
	}{
		{"signed_64KiB", signedFrame(64 * 1024), VariantSigned},
		{"signed_1B", signedFrame(1), VariantSigned},
		{"unsigned_two_chunks", unsignedFrame(64), VariantUnsignedTrailer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Detect(tc.head); got != tc.want {
				t.Errorf("Detect = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetect_NegativeCases verifies legitimate or malformed inputs do not
// trip detection.
func TestDetect_NegativeCases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		head []byte
	}{
		{"empty", nil},
		{"plain_text", []byte("hello world\nthis is a normal file")},
		{"hex_prefix_text", []byte("deadbeef\r\ndata that is not chunked")},
		{"single_unsigned_chunk", []byte("ff\r\n" + string(make([]byte, 0xff)) + "\r\n")},
		{"signed_truncated_signature", []byte("100;chunk-signature=abc\r\n")},
		{"signed_no_crlf", []byte("100;chunk-signature=" + string(make([]byte, 64)))},
		{"unsigned_oversized", []byte("ffffffff\r\n")},
		{"unsigned_negative_chunk_size", []byte("-1\r\n")},
		{"unsigned_chunk_lying_about_size", []byte("100\r\nshort\r\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Detect(tc.head); got != VariantNone {
				t.Errorf("Detect = %q, want VariantNone", got)
			}
		})
	}
}

// TestDetect_UnsignedZeroAfterFirst accepts a final zero-size chunk after
// at least one real chunk because that is a valid termination of the
// framing.
func TestDetect_UnsignedZeroAfterFirst(t *testing.T) {
	t.Parallel()
	head := []byte("4\r\nABCD\r\n0\r\n")
	if got := Detect(head); got != VariantUnsignedTrailer {
		t.Errorf("Detect = %q, want VariantUnsignedTrailer", got)
	}
}
