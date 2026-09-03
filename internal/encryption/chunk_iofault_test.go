// -------------------------------------------------------------------------------
// Chunk Reader IO-Fault Tests
//
// Author: Alex Freidah
//
// Asserts the streaming encrypt/decrypt readers propagate non-EOF source
// errors instead of silently returning io.EOF. A truncated stream that
// reports clean EOF is indistinguishable from a complete stream to
// downstream consumers (replicator, scrubber, GET proxy) and is the
// failure mode this test pins.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// -------------------------------------------------------------------------
// CONSTANTS
// -------------------------------------------------------------------------

// errFaultyRead is the synthetic error returned by faultyReader. The
// concrete type is irrelevant; the test only requires that the error is
// not io.EOF.
var errFaultyRead = errors.New("synthetic transport failure")

// faultyReader returns errFaultyRead with n=0 on the first Read call.
// Mirrors a transient backend failure mid-stream.
type faultyReader struct {
	called bool
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Read implements io.Reader.
func (f *faultyReader) Read(_ []byte) (int, error) {
	if f.called {
		return 0, io.EOF
	}
	f.called = true
	return 0, errFaultyRead
}

// TestEncryptReader_PropagatesNonEOFError feeds a faulty source into
// newEncryptReader and asserts the underlying error reaches the caller
// instead of being squashed to io.EOF.
func TestEncryptReader_PropagatesNonEOFError(t *testing.T) {
	t.Parallel()
	dek := bytes.Repeat([]byte{0xab}, 32)
	er, err := newEncryptReader(&faultyReader{}, dek, 64, newChunkBuffers(64), nil)
	if err != nil {
		t.Fatalf("newEncryptReader: %v", err)
	}
	// Drain the header (deterministic 32 bytes) so the next Read pulls
	// from the source and triggers the fault.
	hdr := make([]byte, HeaderSize)
	if _, err := io.ReadFull(er, hdr); err != nil {
		t.Fatalf("read header: %v", err)
	}
	buf := make([]byte, 4096)
	_, err = er.Read(buf)
	if errors.Is(err, io.EOF) {
		t.Fatal("encryptReader returned io.EOF; expected the underlying error to propagate")
	}
	if !errors.Is(err, errFaultyRead) {
		t.Errorf("err = %v, want errFaultyRead", err)
	}
}

// TestDecryptReader_PropagatesNonEOFError feeds a faulty source into
// newDecryptReader and asserts the underlying error reaches the caller
// instead of being squashed to io.EOF.
func TestDecryptReader_PropagatesNonEOFError(t *testing.T) {
	t.Parallel()
	dek := bytes.Repeat([]byte{0xab}, 32)
	baseNonce := make([]byte, NonceSize)
	dr, err := newDecryptReader(&faultyReader{}, dek, baseNonce, 64, 0, newChunkBuffers(64), nil)
	if err != nil {
		t.Fatalf("newDecryptReader: %v", err)
	}
	buf := make([]byte, 4096)
	_, err = dr.Read(buf)
	if errors.Is(err, io.EOF) {
		t.Fatal("decryptReader returned io.EOF; expected the underlying error to propagate")
	}
	if !errors.Is(err, errFaultyRead) {
		t.Errorf("err = %v, want errFaultyRead", err)
	}
}

// TestEncryptReader_CleanEOFStillEnds confirms the fix does not regress
// the clean-end-of-stream path: a source that returns io.EOF immediately
// must still produce a clean EOF (after the header is drained).
func TestEncryptReader_CleanEOFStillEnds(t *testing.T) {
	t.Parallel()
	dek := bytes.Repeat([]byte{0xab}, 32)
	er, err := newEncryptReader(strings.NewReader(""), dek, 64, newChunkBuffers(64), nil)
	if err != nil {
		t.Fatalf("newEncryptReader: %v", err)
	}
	out, err := io.ReadAll(er)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(out) != HeaderSize {
		t.Errorf("expected header-only output, got %d bytes", len(out))
	}
}
