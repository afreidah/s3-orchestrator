// -------------------------------------------------------------------------------
// Chunk-Framing Detection
//
// Author: Alex Freidah
//
// Pure helpers that classify the head of an object body as AWS chunked
// encoding framing. Used by the unit tests in this package and by the
// build-tagged TestScanChunkedFraming live-cluster diagnostic to flag
// objects that ended up storing the on-the-wire framing of a streaming
// SigV4 PUT instead of the decoded payload.
// -------------------------------------------------------------------------------

package chunkframing

import (
	"bufio"
	"bytes"
	"errors"
	"regexp"
	"strconv"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// Variant names a recognised flavour of aws-chunked framing.
type Variant string

const (
	// VariantNone indicates the head bytes do not look like chunked framing.
	VariantNone Variant = ""
	// VariantSigned matches the leading line of STREAMING-AWS4-HMAC-SHA256-
	// PAYLOAD or its TRAILER-suffixed sibling.
	VariantSigned Variant = "signed"
	// VariantUnsignedTrailer matches the bare-hex chunk framing emitted by
	// STREAMING-UNSIGNED-PAYLOAD-TRAILER.
	VariantUnsignedTrailer Variant = "unsigned_trailer"
)

// MaxChunkSizeBytes caps a single declared chunk during validation. AWS
// SDKs default to 64 KiB; values beyond this threshold are treated as a
// negative signal.
const MaxChunkSizeBytes = 16 * 1024 * 1024

// MinProbeBytes is the minimum head-window size required to validate the
// unsigned variant across two consecutive 64 KiB chunks.
const MinProbeBytes = 64*1024 + 1024

// signedChunkHeader matches the leading line of a signed aws-chunked body:
// <hex-size>;chunk-signature=<64-hex>\r\n.
var signedChunkHeader = regexp.MustCompile(`^[0-9a-fA-F]{1,8};chunk-signature=[0-9a-fA-F]{64}\r\n`)

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Detect classifies the head of an object body. Returns the recognised
// variant when bytes match a chunked-framing layout, or VariantNone when
// they look like ordinary content.
func Detect(head []byte) Variant {
	if signedChunkHeader.Match(head) {
		return VariantSigned
	}
	if validUnsignedChain(head) {
		return VariantUnsignedTrailer
	}
	return VariantNone
}

// -------------------------------------------------------------------------
// INTERNALS
// -------------------------------------------------------------------------

// validUnsignedChain parses head bytes as two consecutive aws-chunked
// unsigned chunks. Validating two chunks (rather than one) eliminates false
// positives on files that happen to start with hex digits followed by CRLF
// because the second chunk header must land at exactly the byte offset
// declared by the first chunk size.
func validUnsignedChain(head []byte) bool {
	br := bufio.NewReader(bytes.NewReader(head))
	for chunk := range 2 {
		size, err := readUnsignedChunkHeader(br)
		if err != nil {
			return false
		}
		if size == 0 {
			return chunk > 0
		}
		if _, err := br.Discard(int(size) + 2); err != nil {
			return false
		}
	}
	return true
}

// readUnsignedChunkHeader reads one CRLF-terminated hex chunk size and
// rejects any line carrying a chunk-signature extension (which belongs to
// the signed variant) or a value outside [0, MaxChunkSizeBytes].
func readUnsignedChunkHeader(br *bufio.Reader) (int64, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return 0, errors.New("missing CRLF terminator")
	}
	hexSize := line[:len(line)-2]
	if bytes.IndexByte([]byte(hexSize), ';') >= 0 {
		return 0, errors.New("chunk extension present")
	}
	size, err := strconv.ParseInt(hexSize, 16, 64)
	if err != nil || size < 0 || size > MaxChunkSizeBytes {
		return 0, errors.New("invalid chunk size")
	}
	return size, nil
}
