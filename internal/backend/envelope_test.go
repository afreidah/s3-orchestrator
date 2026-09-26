// -------------------------------------------------------------------------------
// Envelope Header Fetch Tests
//
// Author: Alex Freidah
//
// Covers a full header, a short object, a zero-byte object the backend answers
// with 416, and a real failure.
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

// TestFetchEnvelopeHeader_ReadsPrefix verifies a long object yields exactly
// the header-sized prefix.
func TestFetchEnvelopeHeader_ReadsPrefix(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	data := bytes.Repeat([]byte("x"), encryption.HeaderSize*2)
	_, _ = mock.PutObject(context.Background(), "key", bytes.NewReader(data), int64(len(data)), "", nil)

	hdr, err := FetchEnvelopeHeader(context.Background(), mock, "key")
	if err != nil {
		t.Fatalf("FetchEnvelopeHeader: %v", err)
	}
	if len(hdr) != encryption.HeaderSize {
		t.Errorf("len = %d, want %d", len(hdr), encryption.HeaderSize)
	}
}

// TestFetchEnvelopeHeader_ShortObject verifies an object shorter than a
// header yields all of its bytes.
func TestFetchEnvelopeHeader_ShortObject(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	_, _ = mock.PutObject(context.Background(), "key", strings.NewReader("abc"), 3, "", nil)

	hdr, err := FetchEnvelopeHeader(context.Background(), mock, "key")
	if err != nil {
		t.Fatalf("FetchEnvelopeHeader: %v", err)
	}
	if string(hdr) != "abc" {
		t.Errorf("header = %q, want %q", hdr, "abc")
	}
}

// TestFetchEnvelopeHeader_ZeroByteObject verifies the 416 a backend returns
// for a zero-byte object reads as an empty header, not an error.
func TestFetchEnvelopeHeader_ZeroByteObject(t *testing.T) {
	t.Parallel()
	mock := newMockBackend()
	mock.getErr = &httpError{code: 416, msg: "InvalidRange"}

	hdr, err := FetchEnvelopeHeader(context.Background(), mock, "empty")
	if err != nil {
		t.Fatalf("FetchEnvelopeHeader on a 416 = %v, want nil", err)
	}
	if len(hdr) != 0 {
		t.Errorf("header = %q, want empty", hdr)
	}
}

// TestFetchEnvelopeHeader_Failure verifies any other error is returned.
func TestFetchEnvelopeHeader_Failure(t *testing.T) {
	t.Parallel()
	cause := &httpError{code: 500, msg: "InternalError"}
	mock := newMockBackend()
	mock.getErr = cause

	if _, err := FetchEnvelopeHeader(context.Background(), mock, "key"); !errors.Is(err, cause) {
		t.Errorf("err = %v, want it to wrap the backend error", err)
	}
}
