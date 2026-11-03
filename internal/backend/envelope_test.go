// -------------------------------------------------------------------------------
// Ranged Read and Envelope Header Tests
//
// Author: Alex Freidah
//
// Covers a full header, a short object, a zero-byte object the backend answers
// with 416, a real failure, and a read capped at its span.
// -------------------------------------------------------------------------------

package backend

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/encryption"
)

// TestReadEnvelopeHeader_ReadsPrefix verifies a long object yields exactly
// the header-sized prefix.
func TestReadEnvelopeHeader_ReadsPrefix(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	data := bytes.Repeat([]byte("x"), encryption.HeaderSize*2)
	_, _ = mock.PutObject(context.Background(), "key", bytes.NewReader(data), int64(len(data)), "", nil)

	hdr, err := ReadEnvelopeHeader(context.Background(), DirectReader{Backend: mock}, "key")
	if err != nil {
		t.Fatalf("ReadEnvelopeHeader: %v", err)
	}
	if len(hdr) != encryption.HeaderSize {
		t.Errorf("len = %d, want %d", len(hdr), encryption.HeaderSize)
	}
}

// TestReadEnvelopeHeader_ShortObject verifies an object shorter than a
// header yields all of its bytes.
func TestReadEnvelopeHeader_ShortObject(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	_, _ = mock.PutObject(context.Background(), "key", strings.NewReader("abc"), 3, "", nil)

	hdr, err := ReadEnvelopeHeader(context.Background(), DirectReader{Backend: mock}, "key")
	if err != nil {
		t.Fatalf("ReadEnvelopeHeader: %v", err)
	}
	if string(hdr) != "abc" {
		t.Errorf("header = %q, want %q", hdr, "abc")
	}
}

// TestReadEnvelopeHeader_ZeroByteObject verifies the 416 a backend returns
// for a zero-byte object reads as an empty header, not an error.
func TestReadEnvelopeHeader_ZeroByteObject(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 416, msg: "InvalidRange"}

	hdr, err := ReadEnvelopeHeader(context.Background(), DirectReader{Backend: mock}, "empty")
	if err != nil {
		t.Fatalf("ReadEnvelopeHeader on a 416 = %v, want nil", err)
	}
	if len(hdr) != 0 {
		t.Errorf("header = %q, want empty", hdr)
	}
}

// TestReadEnvelopeHeader_Failure verifies any other error is returned.
func TestReadEnvelopeHeader_Failure(t *testing.T) {
	t.Parallel()
	cause := &httpError{code: 500, msg: "InternalError"}
	mock := newMockBackend()
	mock.getErr = cause

	if _, err := ReadEnvelopeHeader(context.Background(), DirectReader{Backend: mock}, "key"); !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to wrap the backend error", err)
	}
}

// TestReadSpan_CapsAtTheSpan verifies a body longer than the span asked for,
// as from a backend that ignores Range, is read only up to the span.
func TestReadSpan_CapsAtTheSpan(t *testing.T) {
	t.Parallel()
	got, err := ReadSpan(strings.NewReader("abcdefgh"), 2, 4)
	if err != nil || string(got) != "abc" {
		t.Errorf("ReadSpan = %q, %v; want the first 3 bytes", got, err)
	}
}
