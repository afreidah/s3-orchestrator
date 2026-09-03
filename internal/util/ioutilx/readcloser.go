// -------------------------------------------------------------------------------
// ioutilx - Read-Closer Composition Helpers
//
// Author: Alex Freidah
//
// Generic stream-composition utilities shared by the proxy read paths,
// encryption wrappers, and backend response handlers. Keeping these here
// avoids each package growing its own slightly different version of
// "wrap this reader but keep closing the original body."
// -------------------------------------------------------------------------------

package ioutilx

import (
	"io"
	"sync"
)

// ReadCloser returns an io.ReadCloser that reads from r and delegates
// Close to c. Use when a transformation wraps a reader (decryption,
// metric counting, tee) but the original closer still has to run so
// the underlying resource (HTTP body, file, etc.) is released.
func ReadCloser(r io.Reader, c io.Closer) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{Reader: r, Closer: c}
}

// WithCancel wraps rc so Close calls both rc.Close and cancel. cancel
// is guarded by sync.Once, so repeated Close calls only invoke it
// once. Use to release per-call timeout contexts when the consumer
// finishes reading the body, rather than waiting for the deadline.
func WithCancel(rc io.ReadCloser, cancel func()) io.ReadCloser {
	return &cancellingReadCloser{ReadCloser: rc, cancel: cancel}
}

// cancellingReadCloser invokes cancel exactly once after the wrapped
// ReadCloser is closed. Idempotent so callers can defer Close without
// worrying about double-cancel.
type cancellingReadCloser struct {
	io.ReadCloser
	cancel func()
	once   sync.Once
}

// Close closes the wrapped ReadCloser, then invokes cancel. The error
// from the underlying Close is returned; cancel cannot fail.
func (c *cancellingReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}
