// -------------------------------------------------------------------------------
// Envelope Detection Tests
//
// Author: Alex Freidah
//
// Coverage for the signature check callers use to tell a stored envelope from
// stored plaintext, including that PeekEnvelope always replays the bytes it
// consumed so an inspected stream is still readable from the start.
// -------------------------------------------------------------------------------

package encryption

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestHasEnvelopeMagic verifies the signature check accepts only a full
// leading signature and never reads past a short buffer.
func TestHasEnvelopeMagic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{"exact signature", []byte("SENC"), true},
		{"signature with payload", []byte("SENC\x01rest"), true},
		{"plaintext", []byte("hello world"), false},
		{"truncated signature", []byte("SEN"), false},
		{"empty", nil, false},
		{"signature not at the start", []byte("xSENC"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HasEnvelopeMagic(tt.in); got != tt.want {
				t.Errorf("HasEnvelopeMagic(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestPeekEnvelope verifies detection and, just as importantly, that the
// returned reader still yields the whole original stream.
func TestPeekEnvelope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"envelope", "SENC\x01payload bytes", true},
		{"plaintext", "just some plaintext", false},
		{"shorter than the signature", "ab", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, replayed, err := PeekEnvelope(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("PeekEnvelope: %v", err)
			}
			if got != tt.want {
				t.Errorf("isEnvelope = %v, want %v", got, tt.want)
			}
			rest, err := io.ReadAll(replayed)
			if err != nil {
				t.Fatalf("read replayed: %v", err)
			}
			if string(rest) != tt.in {
				t.Errorf("replayed %q, want the original %q", rest, tt.in)
			}
		})
	}
}

// TestPeekEnvelope_ReadError verifies a mid-signature read failure surfaces
// rather than being silently reported as plaintext.
func TestPeekEnvelope_ReadError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("backend went away")
	r := io.MultiReader(bytes.NewReader([]byte("SE")), &errReader{err: sentinel})
	got, _, err := PeekEnvelope(r)
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the read error, got %v", err)
	}
	if got {
		t.Error("a failed peek must not report an envelope")
	}
}

// errReader fails every read with a fixed error.
type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
