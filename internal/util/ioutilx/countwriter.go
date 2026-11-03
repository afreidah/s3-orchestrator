// -------------------------------------------------------------------------------
// CountingWriter - Byte Count of a Write Stream
//
// Author: Alex Freidah
//
// A writer that forwards to another and counts what it accepted, for a caller
// that needs the length of a stream it never holds whole.
// -------------------------------------------------------------------------------

package ioutilx

import "io"

// CountingWriter forwards writes to W and counts in N the bytes W accepted.
type CountingWriter struct {
	W io.Writer
	N int64
}

// Write forwards p to W and adds what W accepted to N.
func (c *CountingWriter) Write(p []byte) (int, error) {
	n, err := c.W.Write(p)
	c.N += int64(n)
	return n, err
}
