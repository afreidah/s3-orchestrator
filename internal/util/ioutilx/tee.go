// -------------------------------------------------------------------------------
// ioutilx - Completion Tee
//
// Author: Alex Freidah
//
// A body streamed to a client is sometimes also wanted whole somewhere else: a
// cache keeps the bytes, an integrity check hashes them. Either is only
// meaningful for a stream that delivered every byte, so the tee acts only on a
// clean, complete read and never on a body abandoned or cut short partway.
// -------------------------------------------------------------------------------

package ioutilx

import (
	"errors"
	"io"
)

// TeeOnComplete returns rc with every byte read also written to w, and calls
// done once, when the read reaches EOF having delivered exactly expected
// bytes. Any other ending abandons the tee and done never runs: a Close before
// EOF, a read error, a short read, a read past expected, or a write to w that
// fails. The consumer's stream is never affected by the tee.
func TeeOnComplete(rc io.ReadCloser, expected int64, w io.Writer, done func()) io.ReadCloser {
	return &completionTee{ReadCloser: rc, w: w, remaining: expected, done: done}
}

// completionTee is the reader TeeOnComplete returns. A nil done records that
// the tee has finished or been abandoned.
type completionTee struct {
	io.ReadCloser
	w         io.Writer
	remaining int64
	done      func()
}

// Read forwards to the wrapped body, copies what it returned into w, and runs
// done on a clean EOF at exactly the expected length.
func (t *completionTee) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if t.done == nil {
		return n, err
	}
	if n > 0 {
		t.remaining -= int64(n)
		if t.remaining < 0 {
			t.done = nil
			return n, err
		}
		if _, werr := t.w.Write(p[:n]); werr != nil {
			t.done = nil
			return n, err
		}
	}
	switch {
	case errors.Is(err, io.EOF) && t.remaining == 0:
		done := t.done
		t.done = nil
		done()
	case err != nil:
		t.done = nil
	}
	return n, err
}

// Close abandons a tee that has not completed and closes the wrapped body.
func (t *completionTee) Close() error {
	t.done = nil
	return t.ReadCloser.Close()
}
