// -------------------------------------------------------------------------------
// Streaming SigV4 Chunk Reader
//
// Author: Alex Freidah
//
// Validates and decodes the body of an AWS streaming-payload PUT. The
// canonical-request seed signature only authenticates the request envelope
// (headers and the STREAMING-* sentinel as the payload-hash placeholder);
// the actual payload integrity is carried by per-chunk signatures or by a
// signed trailer block. NewChunkReader wraps the request body with a
// verifying decoder that yields the decoded user payload to the handler
// and returns a typed error if any chunk or trailer signature fails.
//
// Three streaming variants are supported, matching the AWS S3 spec:
//
//	STREAMING-AWS4-HMAC-SHA256-PAYLOAD          (chunks signed)
//	STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER  (chunks signed + trailer)
//	STREAMING-UNSIGNED-PAYLOAD-TRAILER          (chunks unsigned + trailer)
//
// One chunk is held in memory at a time. Chunk size is capped to defeat
// declared-but-undelivered DoS, and the chunk-header line, trailer block,
// and trailer-header count are independently bounded.
// -------------------------------------------------------------------------------

package auth

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// MaxChunkBytes is the largest single declared chunk size accepted by the
// reader. AWS SDKs use 64 KiB by default; the cap defends against
// adversarial declarations of huge chunks that would never be delivered.
const MaxChunkBytes = 16 * 1024 * 1024

// maxHeaderLineBytes caps the chunk-size header line (the part before the
// CRLF that terminates the size line, including any chunk-signature
// extension).
const maxHeaderLineBytes = 1024

// maxTrailerBlockBytes caps the total size of the trailer block following
// the zero-size chunk (each header line plus the closing CRLF).
const maxTrailerBlockBytes = 8 * 1024

// maxTrailerHeaders caps the number of declared trailer header names.
const maxTrailerHeaders = 16

// emptyStringSHA256 is hex(SHA256("")), used in the per-chunk
// string-to-sign as the hash of the empty fixed field.
const emptyStringSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// trailerSignatureHeader is the HTTP-style header name carrying the
// trailer signature in trailer variants.
const trailerSignatureHeader = "x-amz-trailer-signature"

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// StreamingVariant identifies which AWS streaming-payload mode a request
// declares via X-Amz-Content-Sha256.
type StreamingVariant int

const (
	// StreamingNone indicates the request body is not a streaming
	// payload; the regular SigV4 payload-hash applies.
	StreamingNone StreamingVariant = iota
	// StreamingSigned: STREAMING-AWS4-HMAC-SHA256-PAYLOAD. Chunks carry
	// per-chunk signatures chained from the seed signature; no trailer.
	StreamingSigned
	// StreamingSignedTrailer: STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER.
	// Chunks carry per-chunk signatures and a signed trailer block
	// follows the zero-size chunk.
	StreamingSignedTrailer
	// StreamingUnsignedTrailer: STREAMING-UNSIGNED-PAYLOAD-TRAILER.
	// Chunks are not authenticated; only the trailer block is signed.
	StreamingUnsignedTrailer
)

// StreamingMaterial is the data the chunk reader needs to verify the
// chain. Built by the auth layer immediately after the seed signature
// verifies; carries the same signing-key derivation the seed used so the
// chain is bound to the request.
type StreamingMaterial struct {
	Variant      StreamingVariant
	SeedSig      string
	SigningKey   []byte
	CredScope    string
	AmzDate      string
	DecodedLen   int64
	TrailerNames []string // sorted, lowercased; only used for trailer variants
}

// Label returns a stable, lowercase identifier for the variant suitable
// as a Prometheus label value. The mapping is fixed across releases so
// dashboards and alert rules can match by variant without ambiguity.
func (v StreamingVariant) Label() string {
	switch v {
	case StreamingSigned:
		return "signed"
	case StreamingSignedTrailer:
		return "signed_trailer"
	case StreamingUnsignedTrailer:
		return "unsigned_trailer"
	default:
		return "none"
	}
}

// -------------------------------------------------------------------------
// ERRORS
// -------------------------------------------------------------------------

// ErrChunkSignatureMismatch indicates a per-chunk signature did not match
// the value computed from the chain. Maps to S3 SignatureDoesNotMatch.
var ErrChunkSignatureMismatch = errors.New("chunk signature mismatch")

// ErrTrailerSignatureMismatch indicates the trailer block's signature did
// not match the value computed from the chain. Maps to S3
// SignatureDoesNotMatch.
var ErrTrailerSignatureMismatch = errors.New("trailer signature mismatch")

// ErrChunkMalformed indicates the chunk framing did not parse: missing
// CRLF, malformed hex, missing chunk-signature, premature EOF, etc. Maps
// to S3 InvalidRequest.
var ErrChunkMalformed = errors.New("malformed chunk framing")

// ErrChunkTooLarge indicates a declared chunk size exceeded MaxChunkBytes.
// Maps to S3 InvalidRequest.
var ErrChunkTooLarge = errors.New("chunk size exceeds limit")

// ErrDecodedLengthMismatch indicates the running total of chunk bytes
// disagreed with x-amz-decoded-content-length. Maps to S3 IncompleteBody.
var ErrDecodedLengthMismatch = errors.New("decoded length does not match declared")

// ErrTrailerMalformed indicates the trailer block did not parse: missing
// declared trailer header, missing trailer signature, oversized block.
// Maps to S3 InvalidRequest.
var ErrTrailerMalformed = errors.New("malformed trailer block")

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// DetectStreamingVariant maps the X-Amz-Content-Sha256 value to a
// streaming variant. Returns StreamingNone when the value is empty or a
// regular hex payload hash.
func DetectStreamingVariant(payloadHash string) StreamingVariant {
	switch payloadHash {
	case "STREAMING-AWS4-HMAC-SHA256-PAYLOAD":
		return StreamingSigned
	case "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER":
		return StreamingSignedTrailer
	case "STREAMING-UNSIGNED-PAYLOAD-TRAILER":
		return StreamingUnsignedTrailer
	default:
		return StreamingNone
	}
}

// streamingMaterialFromRequest returns a StreamingMaterial when the
// request declares a streaming payload, or (nil, nil) when the request
// is a regular non-streaming SigV4 PUT. Returns an error when the
// streaming-required headers (Content-Encoding: aws-chunked,
// x-amz-decoded-content-length, and x-amz-trailer for trailer variants)
// are missing or malformed.
func streamingMaterialFromRequest(r *http.Request, seedSig string, signingKey []byte, credScope, amzDate string) (*StreamingMaterial, error) {
	variant := DetectStreamingVariant(r.Header.Get("X-Amz-Content-Sha256"))
	if variant == StreamingNone {
		return nil, nil
	}
	if !hasAwsChunkedEncoding(r.Header.Get("Content-Encoding")) {
		return nil, fmt.Errorf("streaming payload requires Content-Encoding: aws-chunked")
	}
	decoded, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
	if err != nil || decoded < 0 {
		return nil, fmt.Errorf("invalid x-amz-decoded-content-length")
	}
	var trailerNames []string
	if variant == StreamingSignedTrailer || variant == StreamingUnsignedTrailer {
		trailerNames = parseTrailerNames(r.Header.Get("X-Amz-Trailer"))
		if len(trailerNames) == 0 {
			return nil, fmt.Errorf("trailer streaming requires x-amz-trailer header")
		}
	}
	return &StreamingMaterial{
		Variant:      variant,
		SeedSig:      seedSig,
		SigningKey:   signingKey,
		CredScope:    credScope,
		AmzDate:      amzDate,
		DecodedLen:   decoded,
		TrailerNames: trailerNames,
	}, nil
}

// hasAwsChunkedEncoding reports whether the Content-Encoding header
// contains aws-chunked (case-insensitive, comma-separated list per RFC
// 7231).
func hasAwsChunkedEncoding(value string) bool {
	for part := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "aws-chunked") {
			return true
		}
	}
	return false
}

// parseTrailerNames splits a comma-separated x-amz-trailer header into a
// sorted, lowercased list with empty entries dropped.
func parseTrailerNames(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// NewChunkReader wraps body with a verifying decoder. The returned reader
// yields decoded user payload bytes only; chunk framing is consumed and
// validated as part of each Read. After EOF the reader has fully verified
// the chain, including the trailer block where applicable. mat is taken
// by pointer to avoid copying the embedded signing key on every request.
func NewChunkReader(body io.ReadCloser, mat *StreamingMaterial) io.ReadCloser {
	return &chunkReader{
		src:           bufio.NewReaderSize(body, 64*1024),
		closer:        body,
		variant:       mat.Variant,
		prevSig:       mat.SeedSig,
		signingKey:    mat.SigningKey,
		credScope:     mat.CredScope,
		amzDate:       mat.AmzDate,
		decodedTarget: mat.DecodedLen,
		trailerNames:  mat.TrailerNames,
	}
}

// -------------------------------------------------------------------------
// READER
// -------------------------------------------------------------------------

// chunkReader holds one chunk's decoded body in flight at a time.
// Signature verification happens when the chunk has been fully read from
// the wire and before any byte is delivered to the caller.
type chunkReader struct {
	src           *bufio.Reader
	closer        io.Closer
	variant       StreamingVariant
	prevSig       string
	signingKey    []byte
	credScope     string
	amzDate       string
	decodedTarget int64
	trailerNames  []string

	buf     []byte // verified bytes pending delivery
	decoded int64
	eof     bool
	err     error // sticky terminal error
}

// Read delivers verified payload bytes. Each call may transparently load
// the next chunk from the wire, validate its signature, and return its
// body. Returns io.EOF after the zero-size chunk and trailer (if any) have
// been validated.
func (r *chunkReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	for len(r.buf) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		if err := r.advance(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// Close closes the underlying body. The caller is expected to have read
// to EOF; remaining bytes (if any) are not drained because the wrapping
// HTTP server does that on its own when the handler returns.
func (r *chunkReader) Close() error {
	return r.closer.Close()
}

// -------------------------------------------------------------------------
// CHUNK PARSING
// -------------------------------------------------------------------------

// advance reads the next chunk header, fully buffers its body, validates
// the signature, and stores the body in r.buf. When the size==0 chunk is
// reached, advance runs trailer validation and the decoded-length check
// and sets r.eof.
func (r *chunkReader) advance() error {
	size, chunkSig, err := r.readChunkHeader()
	if err != nil {
		return err
	}
	if size == 0 {
		return r.finalize(chunkSig)
	}
	if size > MaxChunkBytes {
		return ErrChunkTooLarge
	}
	if r.decoded+size > r.decodedTarget {
		return ErrDecodedLengthMismatch
	}

	body := make([]byte, size)
	if _, err := io.ReadFull(r.src, body); err != nil {
		return ErrChunkMalformed
	}
	if err := r.expectCRLF(); err != nil {
		return err
	}

	if r.variant == StreamingSigned || r.variant == StreamingSignedTrailer {
		expected := r.computeChunkSig(body)
		if !hmac.Equal([]byte(expected), []byte(chunkSig)) {
			return ErrChunkSignatureMismatch
		}
		r.prevSig = expected
	}

	r.buf = body
	r.decoded += size
	return nil
}

// readChunkHeader reads one chunk-size line and returns the parsed size
// plus the chunk-signature extension when the variant requires one. For
// unsigned variants the second return is empty and any extension on the
// line is rejected.
func (r *chunkReader) readChunkHeader() (int64, string, error) {
	line, err := readBoundedLine(r.src, maxHeaderLineBytes)
	if err != nil {
		return 0, "", err
	}
	hexSize, ext, hasExt := strings.Cut(line, ";")
	hexSize = strings.TrimSpace(hexSize)
	size, err := strconv.ParseInt(hexSize, 16, 64)
	if err != nil || size < 0 {
		return 0, "", ErrChunkMalformed
	}

	signed := r.variant == StreamingSigned || r.variant == StreamingSignedTrailer
	switch {
	case signed && !hasExt:
		return 0, "", ErrChunkMalformed
	case !signed && hasExt:
		return 0, "", ErrChunkMalformed
	case signed:
		const prefix = "chunk-signature="
		if !strings.HasPrefix(ext, prefix) {
			return 0, "", ErrChunkMalformed
		}
		sig := ext[len(prefix):]
		if len(sig) != 64 || !isHex(sig) {
			return 0, "", ErrChunkMalformed
		}
		return size, sig, nil
	default:
		return size, "", nil
	}
}

// expectCRLF reads exactly two bytes and rejects anything other than a
// literal CRLF. Bare LF is rejected by spec.
func (r *chunkReader) expectCRLF() error {
	var pair [2]byte
	if _, err := io.ReadFull(r.src, pair[:]); err != nil {
		return ErrChunkMalformed
	}
	if pair[0] != '\r' || pair[1] != '\n' {
		return ErrChunkMalformed
	}
	return nil
}

// computeChunkSig builds the per-chunk string-to-sign and returns the
// hex-encoded HMAC-SHA256 over it.
func (r *chunkReader) computeChunkSig(body []byte) string {
	chunkHash := sha256.Sum256(body)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		r.amzDate,
		r.credScope,
		r.prevSig,
		emptyStringSHA256,
		hex.EncodeToString(chunkHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, r.signingKey)
	mac.Write([]byte(stringToSign))
	return hex.EncodeToString(mac.Sum(nil))
}

// -------------------------------------------------------------------------
// FINALIZE
// -------------------------------------------------------------------------

// finalize handles the zero-size chunk. For signed variants it verifies
// the final chunk signature (computed over the empty body). For trailer
// variants it parses and validates the trailer block. In all paths it
// asserts that the running decoded length equals the declared length.
func (r *chunkReader) finalize(finalChunkSig string) error {
	signed := r.variant == StreamingSigned || r.variant == StreamingSignedTrailer
	if signed {
		expected := r.computeChunkSig(nil)
		if !hmac.Equal([]byte(expected), []byte(finalChunkSig)) {
			return ErrChunkSignatureMismatch
		}
		r.prevSig = expected
	}

	switch r.variant {
	case StreamingSignedTrailer, StreamingUnsignedTrailer:
		canonical, providedSig, err := r.readTrailerBlock()
		if err != nil {
			return err
		}
		expected := r.computeTrailerSig(canonical)
		if !hmac.Equal([]byte(expected), []byte(providedSig)) {
			return ErrTrailerSignatureMismatch
		}
	case StreamingSigned:
		if err := r.expectCRLF(); err != nil {
			return err
		}
	}

	if r.decoded != r.decodedTarget {
		return ErrDecodedLengthMismatch
	}
	r.eof = true
	return nil
}

// trailerKV holds one parsed trailer header line.
type trailerKV struct{ name, value string }

// readTrailerBlock reads the trailer headers up to the closing empty
// line, returning the canonicalized trailer headers (sorted, lowercased,
// trimmed values, joined as "name:value\n") and the trailer signature.
// Header lines for x-amz-trailer-signature do not appear in the canonical
// form because they carry the signature output, not an input.
func (r *chunkReader) readTrailerBlock() (string, string, error) {
	headers, trailerSig, err := r.parseTrailerLines()
	if err != nil {
		return "", "", err
	}
	if trailerSig == "" {
		return "", "", ErrTrailerMalformed
	}
	if !declaredTrailersPresent(r.trailerNames, headers) {
		return "", "", ErrTrailerMalformed
	}
	return canonicalizeTrailers(headers), trailerSig, nil
}

// parseTrailerLines reads CRLF-terminated header lines until the
// closing empty line, splitting parsed headers from the trailer
// signature value.
func (r *chunkReader) parseTrailerLines() ([]trailerKV, string, error) {
	headers := make([]trailerKV, 0, len(r.trailerNames))
	var trailerSig string
	var consumed int
	for {
		line, err := readBoundedLine(r.src, maxHeaderLineBytes)
		if err != nil {
			return nil, "", ErrTrailerMalformed
		}
		consumed += len(line) + 2 // +2 for CRLF
		if consumed > maxTrailerBlockBytes {
			return nil, "", ErrTrailerMalformed
		}
		if line == "" {
			return headers, trailerSig, nil
		}
		var ok bool
		headers, trailerSig, ok = appendTrailerLine(line, headers, trailerSig)
		if !ok {
			return nil, "", ErrTrailerMalformed
		}
	}
}

// appendTrailerLine parses one trailer-header line and folds it into
// the running headers slice or, when the line carries the trailer
// signature, into trailerSig. Returns ok=false on malformed input, a
// duplicate trailer signature, or trailer-header overflow.
func appendTrailerLine(line string, headers []trailerKV, trailerSig string) ([]trailerKV, string, bool) {
	name, value, ok := splitTrailerLine(line)
	if !ok {
		return headers, trailerSig, false
	}
	if name == trailerSignatureHeader {
		if trailerSig != "" {
			return headers, trailerSig, false
		}
		return headers, value, true
	}
	if len(headers) >= maxTrailerHeaders {
		return headers, trailerSig, false
	}
	return append(headers, trailerKV{name, value}), trailerSig, true
}

// splitTrailerLine extracts a lowercased name and a trimmed value from
// a single trailer-header line. Returns ok=false when the line carries
// no colon.
func splitTrailerLine(line string) (name, value string, ok bool) {
	rawName, rawValue, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSpace(rawName)), strings.TrimSpace(rawValue), true
}

// declaredTrailersPresent reports whether every trailer name announced
// by the request's x-amz-trailer header arrived in the trailer block.
func declaredTrailersPresent(declared []string, headers []trailerKV) bool {
	for _, name := range declared {
		found := false
		for _, h := range headers {
			if h.name == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// canonicalizeTrailers sorts headers by name and renders them as
// "name:value\n" lines, the form used in the trailer string-to-sign.
func canonicalizeTrailers(headers []trailerKV) string {
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })
	var canonical strings.Builder
	for _, h := range headers {
		canonical.WriteString(h.name)
		canonical.WriteByte(':')
		canonical.WriteString(h.value)
		canonical.WriteByte('\n')
	}
	return canonical.String()
}

// computeTrailerSig builds the trailer string-to-sign and returns the
// hex-encoded HMAC-SHA256 over it.
func (r *chunkReader) computeTrailerSig(canonical string) string {
	canonicalHash := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-TRAILER",
		r.amzDate,
		r.credScope,
		r.prevSig,
		hex.EncodeToString(canonicalHash[:]),
	}, "\n")
	mac := hmac.New(sha256.New, r.signingKey)
	mac.Write([]byte(stringToSign))
	return hex.EncodeToString(mac.Sum(nil))
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// readBoundedLine reads a CRLF-terminated line up to maxLen bytes
// (excluding the terminator) and rejects bare LF and oversized lines.
func readBoundedLine(br *bufio.Reader, maxLen int) (string, error) {
	var buf bytes.Buffer
	for buf.Len() <= maxLen {
		b, err := br.ReadByte()
		if err != nil {
			return "", ErrChunkMalformed
		}
		if b == '\r' {
			next, err := br.ReadByte()
			if err != nil {
				return "", ErrChunkMalformed
			}
			if next != '\n' {
				return "", ErrChunkMalformed
			}
			return buf.String(), nil
		}
		if b == '\n' {
			return "", ErrChunkMalformed
		}
		buf.WriteByte(b)
	}
	return "", fmt.Errorf("%w: header line over %d bytes", ErrChunkMalformed, maxLen)
}

// isHex returns true when s is non-empty and contains only hex digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
