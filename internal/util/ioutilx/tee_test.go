// -------------------------------------------------------------------------------
// ioutilx - Completion Tee Tests
//
// Author: Alex Freidah
//
// Pins the completion contract: done runs once, and only after a clean EOF at
// exactly the expected length. Every other ending abandons the tee without
// changing what the consumer reads.
// -------------------------------------------------------------------------------

package ioutilx_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// teeSource is the body most tests stream through the tee, and teePrefix
// is the part of it a truncated or failing body delivers.
const (
	teeSource = "hello world"
	teePrefix = "hello"
)

// errBackend is the mid-stream failure the error tests inject.
var errBackend = errors.New("backend exploded")

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// errAfterReader returns body and then err on every later Read, standing in
// for a backend stream that fails partway.
type errAfterReader struct {
	body []byte
	pos  int
	err  error
}

// failingWriter rejects every Write, standing in for a tee sink that fails.
type failingWriter struct{}

// closeErrBody is a body whose Close reports err.
type closeErrBody struct {
	io.Reader
	err error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Read returns the remaining body bytes, then err once they are exhausted.
// It never returns bytes and an error from the same call.
func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.body) {
		return 0, r.err
	}
	n := copy(p, r.body[r.pos:])
	r.pos += n
	return n, nil
}

// Write fails without consuming anything.
// The tee must abandon rather than surface this to the consumer.
func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("sink full")
}

// Close returns the configured error.
// It lets a test check the tee forwards the body's Close result.
func (b closeErrBody) Close() error {
	return b.err
}

// -------------------------------------------------------------------------
// HELPERS
// -------------------------------------------------------------------------

// newCountingTee wraps r in a tee expecting expected bytes and returns the tee,
// the sink it writes to, and a pointer to the number of times done ran.
func newCountingTee(r io.Reader, expected int64) (io.ReadCloser, *bytes.Buffer, *int) {
	sink := &bytes.Buffer{}
	calls := new(int)
	tee := ioutilx.TeeOnComplete(io.NopCloser(r), expected, sink, func() { *calls++ })
	return tee, sink, calls
}

// readAllClean drains tee and fails the test on a read error or on output
// that differs from want.
func readAllClean(t *testing.T, tee io.Reader, want string) {
	t.Helper()
	out, err := io.ReadAll(tee)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != want {
		t.Fatalf("consumer read %q, want %q", out, want)
	}
}

// -------------------------------------------------------------------------
// TESTS
// -------------------------------------------------------------------------

// TestTeeOnComplete_CompleteReadCallsDoneOnce covers the one ending that
// completes, across bodies that return EOF with the last bytes, after them, or
// one byte at a time. Reads past EOF and a later Close must not run done again.
func TestTeeOnComplete_CompleteReadCallsDoneOnce(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body func() io.Reader
	}{
		{"eof after data", func() io.Reader { return bytes.NewReader([]byte(teeSource)) }},
		{"eof with data", func() io.Reader { return iotest.DataErrReader(bytes.NewReader([]byte(teeSource))) }},
		{"one byte reads", func() io.Reader { return iotest.OneByteReader(bytes.NewReader([]byte(teeSource))) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tee, sink, calls := newCountingTee(tc.body(), int64(len(teeSource)))

			readAllClean(t, tee, teeSource)
			if n, err := tee.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("Read after EOF = %d, %v; want 0, EOF", n, err)
			}
			if err := tee.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if *calls != 1 {
				t.Errorf("done calls = %d, want 1", *calls)
			}
			if sink.String() != teeSource {
				t.Errorf("sink = %q, want %q", sink.String(), teeSource)
			}
		})
	}
}

// TestTeeOnComplete_ZeroLengthCompletes verifies an empty body announced as
// empty completes on its first EOF.
func TestTeeOnComplete_ZeroLengthCompletes(t *testing.T) {
	t.Parallel()
	tee, _, calls := newCountingTee(bytes.NewReader(nil), 0)
	readAllClean(t, tee, "")
	if *calls != 1 {
		t.Errorf("done calls = %d, want 1", *calls)
	}
}

// TestTeeOnComplete_ShortReadAbandons verifies EOF before the announced length
// never completes, so a truncated body is not taken for the whole one.
func TestTeeOnComplete_ShortReadAbandons(t *testing.T) {
	t.Parallel()
	tee, _, calls := newCountingTee(bytes.NewReader([]byte(teePrefix)), 100)
	readAllClean(t, tee, teePrefix)
	if *calls != 0 {
		t.Errorf("done ran %d times on a short read", *calls)
	}
}

// TestTeeOnComplete_OvershootAbandons verifies a body longer than announced is
// abandoned while the consumer still receives every byte, and the chunk that
// crossed the limit never reaches the sink.
func TestTeeOnComplete_OvershootAbandons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     io.Reader
		wantSink string
	}{
		{"single chunk", bytes.NewReader([]byte(teeSource)), ""},
		{"after a full sink", iotest.OneByteReader(bytes.NewReader([]byte(teeSource))), teePrefix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tee, sink, calls := newCountingTee(tc.body, 5)
			readAllClean(t, tee, teeSource)
			if *calls != 0 {
				t.Errorf("done ran %d times despite overshoot", *calls)
			}
			if sink.String() != tc.wantSink {
				t.Errorf("sink = %q, want %q", sink.String(), tc.wantSink)
			}
		})
	}
}

// TestTeeOnComplete_ReadErrorAbandons verifies a failing body never completes,
// including one that delivered every expected byte before failing, and that
// the consumer sees the body's own error.
func TestTeeOnComplete_ReadErrorAbandons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		expected int64
	}{
		{"mid stream", 100},
		{"after every byte", int64(len(teePrefix))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := &errAfterReader{body: []byte(teePrefix), err: errBackend}
			tee, _, calls := newCountingTee(body, tc.expected)

			out, err := io.ReadAll(tee)
			if !errors.Is(err, errBackend) {
				t.Fatalf("ReadAll err = %v, want %v", err, errBackend)
			}
			if string(out) != teePrefix {
				t.Errorf("consumer read %q, want %q", out, teePrefix)
			}
			if *calls != 0 {
				t.Errorf("done ran %d times despite a read error", *calls)
			}
		})
	}
}

// TestTeeOnComplete_CloseBeforeEOFAbandons covers a client that disconnects:
// once closed early, the tee stays abandoned even if the rest is read later.
func TestTeeOnComplete_CloseBeforeEOFAbandons(t *testing.T) {
	t.Parallel()
	tee, _, calls := newCountingTee(bytes.NewReader([]byte(teeSource)), int64(len(teeSource)))

	if _, err := tee.Read(make([]byte, 4)); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := tee.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	readAllClean(t, tee, teeSource[4:])
	if *calls != 0 {
		t.Errorf("done ran %d times after an early Close", *calls)
	}
}

// TestTeeOnComplete_WriteErrorAbandons verifies a failing sink abandons the tee
// without failing or shortening the consumer's read.
func TestTeeOnComplete_WriteErrorAbandons(t *testing.T) {
	t.Parallel()
	called := false
	body := io.NopCloser(bytes.NewReader([]byte(teeSource)))
	tee := ioutilx.TeeOnComplete(body, int64(len(teeSource)), failingWriter{}, func() { called = true })

	readAllClean(t, tee, teeSource)
	if called {
		t.Error("done ran although the sink rejected the bytes")
	}
}

// TestTeeOnComplete_CloseForwardsError verifies Close returns the wrapped
// body's Close result unchanged.
func TestTeeOnComplete_CloseForwardsError(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("close failed")
	body := closeErrBody{Reader: bytes.NewReader(nil), err: closeErr}
	tee := ioutilx.TeeOnComplete(body, 0, &bytes.Buffer{}, func() {})

	if err := tee.Close(); !errors.Is(err, closeErr) {
		t.Errorf("Close = %v, want %v", err, closeErr)
	}
}
