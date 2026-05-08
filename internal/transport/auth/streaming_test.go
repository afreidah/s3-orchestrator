// -------------------------------------------------------------------------------
// Streaming SigV4 Chunk Reader Tests
//
// Author: Alex Freidah
//
// Table-driven tests for the chunk reader covering all three streaming
// variants. The test fixtures generate framing that exactly matches the
// AWS spec so a successful decode through the reader proves the
// implementation produces the same chain a conforming client signs.
// Negative tests tamper one bit and assert the reader rejects with the
// correct typed error.
// -------------------------------------------------------------------------------

package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// FIXTURE BUILDERS
// -------------------------------------------------------------------------

// fixture holds the inputs required to build a streaming body and the
// material the reader needs to validate it.
type fixture struct {
	variant      StreamingVariant
	signingKey   []byte
	seedSig      string
	credScope    string
	amzDate      string
	trailerNames []string
	trailers     map[string]string
}

// newFixture creates a fixture with deterministic test material.
func newFixture(v StreamingVariant) *fixture {
	return &fixture{ //nolint:gosec // deterministic test fixture; the strings are not real credentials
		variant:    v,
		signingKey: bytes.Repeat([]byte{0xab}, 32),
		seedSig:    strings.Repeat("a", 64),
		credScope:  "20260507/us-east-1/s3/aws4_request",
		amzDate:    "20260507T000000Z",
	}
}

// withTrailers attaches a set of trailer headers; the keys become the
// declared trailer names in the StreamingMaterial.
func (f *fixture) withTrailers(t map[string]string) *fixture {
	f.trailers = t
	names := make([]string, 0, len(t))
	for k := range t {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	f.trailerNames = names
	return f
}

// material returns the StreamingMaterial the reader needs to verify a
// body produced by buildBody.
func (f *fixture) material(decodedLen int64) *StreamingMaterial {
	return &StreamingMaterial{
		Variant:      f.variant,
		SeedSig:      f.seedSig,
		SigningKey:   f.signingKey,
		CredScope:    f.credScope,
		AmzDate:      f.amzDate,
		DecodedLen:   decodedLen,
		TrailerNames: f.trailerNames,
	}
}

// buildBody emits a wire-format streaming body for the fixture's variant
// using the given chunk size to slice the payload.
func (f *fixture) buildBody(payload []byte, chunkSize int) []byte {
	var b bytes.Buffer
	prevSig := f.seedSig
	for off := 0; off < len(payload); off += chunkSize {
		end := min(off+chunkSize, len(payload))
		chunk := payload[off:end]
		f.writeChunk(&b, chunk, &prevSig)
	}
	f.writeChunk(&b, nil, &prevSig)
	if f.variant == StreamingSignedTrailer || f.variant == StreamingUnsignedTrailer {
		f.writeTrailer(&b, prevSig)
	} else {
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// writeChunk writes one chunk frame appropriate for the variant. The
// zero-size terminator chunk omits the body-CRLF because AWS streaming
// places the trailer block (or message-terminating empty line) directly
// after its size line.
func (f *fixture) writeChunk(b *bytes.Buffer, chunk []byte, prevSig *string) {
	signed := f.variant == StreamingSigned || f.variant == StreamingSignedTrailer
	if signed {
		sig := computeChunkSigForTest(f.signingKey, f.amzDate, f.credScope, *prevSig, chunk)
		fmt.Fprintf(b, "%x;chunk-signature=%s\r\n", len(chunk), sig)
		*prevSig = sig
	} else {
		fmt.Fprintf(b, "%x\r\n", len(chunk))
	}
	b.Write(chunk)
	if len(chunk) > 0 {
		b.WriteString("\r\n")
	}
}

// writeTrailer writes the trailer block plus its signature line.
func (f *fixture) writeTrailer(b *bytes.Buffer, prevSig string) {
	canonical := canonicalTrailers(f.trailers)
	sig := computeTrailerSigForTest(f.signingKey, f.amzDate, f.credScope, prevSig, canonical)
	for _, name := range f.trailerNames {
		fmt.Fprintf(b, "%s:%s\r\n", name, f.trailers[name])
	}
	fmt.Fprintf(b, "%s:%s\r\n", trailerSignatureHeader, sig)
	b.WriteString("\r\n")
}

// canonicalTrailers reproduces the canonical trailer-headers form used in
// the trailer string-to-sign: sorted by name, "name:value\n".
func canonicalTrailers(t map[string]string) string {
	names := make([]string, 0, len(t))
	for k := range t {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(t[n]))
		b.WriteByte('\n')
	}
	return b.String()
}

// computeChunkSigForTest mirrors the per-chunk string-to-sign so test
// fixtures produce the same signatures the reader expects.
func computeChunkSigForTest(key []byte, amzDate, scope, prevSig string, body []byte) string {
	chunkHash := sha256.Sum256(body)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		amzDate,
		scope,
		prevSig,
		emptyStringSHA256,
		hex.EncodeToString(chunkHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stringToSign))
	return hex.EncodeToString(mac.Sum(nil))
}

// computeTrailerSigForTest mirrors the trailer string-to-sign.
func computeTrailerSigForTest(key []byte, amzDate, scope, prevSig, canonical string) string {
	canonicalHash := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-TRAILER",
		amzDate,
		scope,
		prevSig,
		hex.EncodeToString(canonicalHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(stringToSign))
	return hex.EncodeToString(mac.Sum(nil))
}

// readAllFromBytes wraps a byte slice in NewChunkReader and reads to EOF.
func readAllFromBytes(t *testing.T, body []byte, mat *StreamingMaterial) ([]byte, error) {
	t.Helper()
	rc := NewChunkReader(io.NopCloser(bytes.NewReader(body)), mat)
	out, err := io.ReadAll(rc)
	_ = rc.Close()
	return out, err
}

// -------------------------------------------------------------------------
// HAPPY PATHS
// -------------------------------------------------------------------------

// TestChunkReader_Signed_HappyPath round-trips bodies of varied size and
// chunk count through the signed variant.
func TestChunkReader_Signed_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		payload   []byte
		chunkSize int
	}{
		{"empty", nil, 64},
		{"single_chunk_11B", []byte("hello world"), 64},
		{"single_chunk_64KiB", bytes.Repeat([]byte{'a'}, 64*1024), 64 * 1024},
		{"multi_chunk", bytes.Repeat([]byte{'b'}, 200*1024), 64 * 1024},
		{"odd_size_split", bytes.Repeat([]byte{'c'}, 12345), 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(StreamingSigned)
			body := f.buildBody(tc.payload, tc.chunkSize)
			got, err := readAllFromBytes(t, body, f.material(int64(len(tc.payload))))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !bytes.Equal(got, tc.payload) {
				t.Errorf("payload mismatch:\nwant len=%d\ngot  len=%d", len(tc.payload), len(got))
			}
		})
	}
}

// TestChunkReader_SignedTrailer_HappyPath round-trips a signed body with
// a trailer block carrying one checksum header.
func TestChunkReader_SignedTrailer_HappyPath(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte{'x'}, 70*1024)
	f := newFixture(StreamingSignedTrailer).withTrailers(map[string]string{
		"x-amz-checksum-crc32": "AAAAAA==",
	})
	body := f.buildBody(payload, 64*1024)
	got, err := readAllFromBytes(t, body, f.material(int64(len(payload))))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch")
	}
}

// TestChunkReader_UnsignedTrailer_HappyPath round-trips an unsigned body
// with a signed trailer.
func TestChunkReader_UnsignedTrailer_HappyPath(t *testing.T) {
	t.Parallel()
	payload := []byte("the quick brown fox jumps over the lazy dog")
	f := newFixture(StreamingUnsignedTrailer).withTrailers(map[string]string{
		"x-amz-checksum-crc32c": "ZZZZZZ==",
	})
	body := f.buildBody(payload, 16)
	got, err := readAllFromBytes(t, body, f.material(int64(len(payload))))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload mismatch:\nwant %q\ngot  %q", payload, got)
	}
}

// -------------------------------------------------------------------------
// NEGATIVE CASES
// -------------------------------------------------------------------------

// TestChunkReader_Signed_TamperedChunk flips a single body byte and
// asserts the chunk-signature check fails.
func TestChunkReader_Signed_TamperedChunk(t *testing.T) {
	t.Parallel()
	payload := []byte("hello world")
	f := newFixture(StreamingSigned)
	body := f.buildBody(payload, 64)
	tampered := flipPayloadByte(t, body)
	_, err := readAllFromBytes(t, tampered, f.material(int64(len(payload))))
	if !errors.Is(err, ErrChunkSignatureMismatch) {
		t.Errorf("err = %v, want ErrChunkSignatureMismatch", err)
	}
}

// TestChunkReader_UnsignedTrailer_TamperedSignature flips a byte in the
// trailer signature and asserts the trailer check fails.
func TestChunkReader_UnsignedTrailer_TamperedSignature(t *testing.T) {
	t.Parallel()
	payload := []byte("payload bytes")
	f := newFixture(StreamingUnsignedTrailer).withTrailers(map[string]string{
		"x-amz-checksum-crc32": "AAAAAA==",
	})
	body := f.buildBody(payload, 64)
	idx := bytes.LastIndex(body, []byte(trailerSignatureHeader+":"))
	if idx < 0 {
		t.Fatalf("could not locate trailer signature in fixture")
	}
	body[idx+len(trailerSignatureHeader)+2] ^= 0x01
	_, err := readAllFromBytes(t, body, f.material(int64(len(payload))))
	if !errors.Is(err, ErrTrailerSignatureMismatch) {
		t.Errorf("err = %v, want ErrTrailerSignatureMismatch", err)
	}
}

// TestChunkReader_Signed_DeclaredLengthMismatch fakes a decoded length
// larger than the actual payload and asserts the reader rejects.
func TestChunkReader_Signed_DeclaredLengthMismatch(t *testing.T) {
	t.Parallel()
	payload := []byte("hello world")
	f := newFixture(StreamingSigned)
	body := f.buildBody(payload, 64)
	mat := f.material(int64(len(payload) + 1))
	_, err := readAllFromBytes(t, body, mat)
	if !errors.Is(err, ErrDecodedLengthMismatch) {
		t.Errorf("err = %v, want ErrDecodedLengthMismatch", err)
	}
}

// TestChunkReader_OversizedChunk rejects a chunk header declaring a size
// above MaxChunkBytes.
func TestChunkReader_OversizedChunk(t *testing.T) {
	t.Parallel()
	body := []byte("ffffffff;chunk-signature=" + strings.Repeat("0", 64) + "\r\n")
	f := newFixture(StreamingSigned)
	_, err := readAllFromBytes(t, body, f.material(0))
	if !errors.Is(err, ErrChunkTooLarge) {
		t.Errorf("err = %v, want ErrChunkTooLarge", err)
	}
}

// TestChunkReader_MalformedFraming covers the most likely framing-shape
// rejections in one table.
func TestChunkReader_MalformedFraming(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []byte
	}{
		{"empty_input", nil},
		{"bare_LF", []byte("4\nABCD\n0\n")},
		{"missing_signature_extension_signed", []byte("4\r\nABCD\r\n0\r\n")},
		{"truncated_chunk_body", []byte("4;chunk-signature=" + strings.Repeat("0", 64) + "\r\nAB")},
		{"non_hex_size", []byte("zz;chunk-signature=" + strings.Repeat("0", 64) + "\r\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(StreamingSigned)
			_, err := readAllFromBytes(t, tc.body, f.material(0))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestChunkReader_TrailerVariants_RejectMissingSignature drops the trailer
// signature line and asserts ErrTrailerMalformed.
func TestChunkReader_TrailerVariants_RejectMissingSignature(t *testing.T) {
	t.Parallel()
	f := newFixture(StreamingUnsignedTrailer).withTrailers(map[string]string{
		"x-amz-checksum-crc32": "AAAAAA==",
	})
	body := f.buildBody([]byte("xx"), 8)
	body = removeTrailerSignature(t, body)
	_, err := readAllFromBytes(t, body, f.material(2))
	if !errors.Is(err, ErrTrailerMalformed) {
		t.Errorf("err = %v, want ErrTrailerMalformed", err)
	}
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// flipPayloadByte locates the first chunk body and toggles one bit; the
// chunk-signature line is intentionally left intact so the chain mismatch
// is detectable.
func flipPayloadByte(t *testing.T, body []byte) []byte {
	t.Helper()
	out := append([]byte(nil), body...)
	// Skip the first chunk header line.
	idx := bytes.Index(out, []byte("\r\n"))
	if idx < 0 {
		t.Fatalf("flipPayloadByte: no CRLF in body")
	}
	if idx+2 >= len(out) {
		t.Fatalf("flipPayloadByte: nothing after first CRLF")
	}
	out[idx+2] ^= 0x01
	return out
}

// removeTrailerSignature strips the x-amz-trailer-signature line so the
// reader sees a trailer block missing its required signature.
func removeTrailerSignature(t *testing.T, body []byte) []byte {
	t.Helper()
	marker := []byte(trailerSignatureHeader + ":")
	start := bytes.Index(body, marker)
	if start < 0 {
		t.Fatalf("removeTrailerSignature: marker not found")
	}
	end := bytes.Index(body[start:], []byte("\r\n"))
	if end < 0 {
		t.Fatalf("removeTrailerSignature: line terminator not found")
	}
	return append(body[:start], body[start+end+2:]...)
}
