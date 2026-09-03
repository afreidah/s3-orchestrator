// -------------------------------------------------------------------------------
// ioutilx - Read-Closer Helper Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package ioutilx_test

import (
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/util/ioutilx"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// trackingCloser counts Close invocations and returns the configured error.
type trackingCloser struct {
	closes atomic.Int32
	err    error
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Close increments the call counter and returns the configured error.
func (c *trackingCloser) Close() error {
	c.closes.Add(1)
	return c.err
}

// TestReadCloser_ReadsFromReader confirms ReadCloser proxies Read to the
// inner reader without touching the closer.
func TestReadCloser_ReadsFromReader(t *testing.T) {
	t.Parallel()
	r := strings.NewReader("hello")
	c := &trackingCloser{}

	rc := ioutilx.ReadCloser(r, c)
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", string(got), "hello")
	}
	if c.closes.Load() != 0 {
		t.Errorf("Close called during Read; calls=%d", c.closes.Load())
	}
}

// TestReadCloser_DelegatesClose confirms Close hits the wrapped closer
// and propagates its error verbatim.
func TestReadCloser_DelegatesClose(t *testing.T) {
	t.Parallel()
	want := errors.New("close failed")
	c := &trackingCloser{err: want}
	rc := ioutilx.ReadCloser(strings.NewReader(""), c)

	if err := rc.Close(); !errors.Is(err, want) {
		t.Fatalf("Close err = %v, want %v", err, want)
	}
	if c.closes.Load() != 1 {
		t.Errorf("inner Close calls = %d, want 1", c.closes.Load())
	}
}

// TestWithCancel_ClosesAndCancels confirms a single Close hits the inner
// closer and fires cancel exactly once.
func TestWithCancel_ClosesAndCancels(t *testing.T) {
	t.Parallel()
	inner := io.NopCloser(strings.NewReader(""))
	var cancels atomic.Int32
	rc := ioutilx.WithCancel(inner, func() { cancels.Add(1) })

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if cancels.Load() != 1 {
		t.Errorf("cancel fired %d times, want 1", cancels.Load())
	}
}

// TestWithCancel_CancelOnlyOnceOnDoubleClose pins the sync.Once
// guarantee callers rely on when deferring Close.
func TestWithCancel_CancelOnlyOnceOnDoubleClose(t *testing.T) {
	t.Parallel()
	var cancels atomic.Int32
	rc := ioutilx.WithCancel(io.NopCloser(strings.NewReader("")), func() { cancels.Add(1) })

	_ = rc.Close()
	_ = rc.Close()

	if cancels.Load() != 1 {
		t.Errorf("cancel fired %d times after double Close, want 1", cancels.Load())
	}
}

// TestWithCancel_PropagatesCloseError confirms WithCancel surfaces the
// inner Close error and still invokes cancel.
func TestWithCancel_PropagatesCloseError(t *testing.T) {
	t.Parallel()
	want := errors.New("body close failed")
	inner := &trackingCloser{err: want}
	var cancels atomic.Int32

	rc := ioutilx.WithCancel(struct {
		io.Reader
		io.Closer
	}{strings.NewReader(""), inner}, func() { cancels.Add(1) })

	if err := rc.Close(); !errors.Is(err, want) {
		t.Fatalf("Close err = %v, want %v", err, want)
	}
	if cancels.Load() != 1 {
		t.Errorf("cancel fired %d times, want 1 (must still fire after Close error)", cancels.Load())
	}
}
