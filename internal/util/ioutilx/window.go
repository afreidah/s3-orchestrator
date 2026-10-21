// -------------------------------------------------------------------------------
// ioutilx - Stream Windows
//
// Author: Alex Freidah
//
// Cuts a byte window out of a forward-only stream: discard up to the start,
// then stop after the window's length. Range decryption and a backend that
// ignores Range both need exactly this over a body that cannot seek.
// -------------------------------------------------------------------------------

package ioutilx

import (
	"errors"
	"io"
)

// Window discards the first skip bytes of r and returns a reader limited to
// the next n. A stream that ends before skip bytes returns io.ErrUnexpectedEOF,
// since the window it was asked for does not exist.
func Window(r io.Reader, skip, n int64) (io.Reader, error) {
	if skip > 0 {
		// CopyN reports io.EOF only when the stream ended short of skip.
		if _, err := io.CopyN(io.Discard, r, skip); errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		} else if err != nil {
			return nil, err
		}
	}
	return io.LimitReader(r, n), nil
}
