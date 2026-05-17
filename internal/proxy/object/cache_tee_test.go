// -------------------------------------------------------------------------------
// Cache Tee Body Tests
//
// Author: Alex Freidah
//
// Pins the streaming-cache contract: clean EOF at the expected size invokes
// the finalizer; any other termination (early Close, short read, backend
// overshoot, mid-read error) drops the buffer and never calls back. These
// tests guard against regressions in #654 - the OOM bug from io.ReadAll
// before the size check.
// -------------------------------------------------------------------------------

package object

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// teeErrReader returns err on the first Read call after returning the seed
// bytes. Used to simulate mid-stream backend failures.
type teeErrReader struct {
	body []byte
	pos  int
	err  error
}

func (r *teeErrReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.body) {
		return 0, r.err
	}
	n := copy(p, r.body[r.pos:])
	r.pos += n
	return n, nil
}

func (r *teeErrReader) Close() error { return nil }

// TestCacheTeeBody_CleanEOF_FinalizesAtExactSize verifies the happy path:
// reading exactly expected bytes followed by EOF triggers onComplete with
// the full body.
func TestCacheTeeBody_CleanEOF_FinalizesAtExactSize(t *testing.T) {
	t.Parallel()
	src := []byte("hello world")
	var got []byte
	tee := newCacheTeeBody(io.NopCloser(bytes.NewReader(src)), int64(len(src)), func(data []byte) {
		got = append(got[:0], data...)
	})

	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("ReadAll = %q, want %q", out, src)
	}
	if !bytes.Equal(got, src) {
		t.Fatalf("finalizer got %q, want %q", got, src)
	}
}

// TestCacheTeeBody_EarlyClose_AbandonsCache verifies Close before EOF
// drops the buffer and skips the finalizer, so partial reads never land
// in the cache as if they were complete.
func TestCacheTeeBody_EarlyClose_AbandonsCache(t *testing.T) {
	t.Parallel()
	src := []byte("hello world")
	called := false
	tee := newCacheTeeBody(io.NopCloser(bytes.NewReader(src)), int64(len(src)), func(data []byte) {
		called = true
	})

	// Read a few bytes then close - simulates client disconnect mid-stream.
	buf := make([]byte, 4)
	if _, err := tee.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := tee.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if called {
		t.Fatal("finalizer invoked on early Close")
	}
}

// TestCacheTeeBody_ShortRead_DoesNotFinalize verifies that EOF at fewer
// than expected bytes (backend lied / truncated) does not finalize. The
// cache must not store a partial object as if it were the full one.
func TestCacheTeeBody_ShortRead_DoesNotFinalize(t *testing.T) {
	t.Parallel()
	src := []byte("hello")
	called := false
	// Announce 100 bytes but the source only has 5.
	tee := newCacheTeeBody(io.NopCloser(bytes.NewReader(src)), 100, func(data []byte) {
		called = true
	})

	out, _ := io.ReadAll(tee)
	if !bytes.Equal(out, src) {
		t.Fatalf("ReadAll = %q, want %q", out, src)
	}
	if called {
		t.Fatal("finalizer invoked on short read (5 bytes vs 100 announced)")
	}
}

// TestCacheTeeBody_Overshoot_AbandonsCache verifies that a backend
// returning more bytes than the announced Content-Length aborts caching
// without breaking the consumer's read - the extra bytes still flow
// through, only the cache write is skipped.
func TestCacheTeeBody_Overshoot_AbandonsCache(t *testing.T) {
	t.Parallel()
	src := []byte("hello world")
	called := false
	// Announce 5 bytes but the source has 11.
	tee := newCacheTeeBody(io.NopCloser(bytes.NewReader(src)), 5, func(data []byte) {
		called = true
	})

	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("ReadAll = %q, want %q (consumer must still receive every byte)", out, src)
	}
	if called {
		t.Fatal("finalizer invoked despite overshoot")
	}
}

// TestCacheTeeBody_MidStreamError_AbandonsCache verifies that a read
// error mid-stream prevents finalization. The error propagates to the
// consumer; the cache stays clean.
func TestCacheTeeBody_MidStreamError_AbandonsCache(t *testing.T) {
	t.Parallel()
	called := false
	body := &teeErrReader{body: []byte("hello"), err: errors.New("backend exploded")}
	tee := newCacheTeeBody(body, 100, func(data []byte) {
		called = true
	})

	_, err := io.ReadAll(tee)
	if err == nil || err.Error() != "backend exploded" {
		t.Fatalf("ReadAll err = %v, want 'backend exploded'", err)
	}
	if called {
		t.Fatal("finalizer invoked despite mid-stream error")
	}
}
