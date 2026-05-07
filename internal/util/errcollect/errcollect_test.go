// -------------------------------------------------------------------------------
// Error Collector Tests
//
// Author: Alex Freidah
//
// Pins the contract: zero appends returns nil, one append returns that
// error verbatim (no wrapping), N appends returns an errors.Join chain
// every appended error stays accessible via errors.Is.
// -------------------------------------------------------------------------------

package errcollect_test

import (
	"errors"
	"testing"

	"github.com/afreidah/s3-orchestrator/internal/util/errcollect"
)

// TestCollector_NoAppendsReturnsNil verifies the zero-value Collector is
// useful and Done returns nil when nothing was appended.
func TestCollector_NoAppendsReturnsNil(t *testing.T) {
	t.Parallel()
	var c errcollect.Collector
	if err := c.Done(); err != nil {
		t.Errorf("Done() with no appends = %v, want nil", err)
	}
}

// TestCollector_NilAppendsIgnored verifies nil errors do not contribute
// so callers can pass any error-returning expression without guarding.
func TestCollector_NilAppendsIgnored(t *testing.T) {
	t.Parallel()
	var c errcollect.Collector
	c.Append(nil)
	c.Append(nil)
	if err := c.Done(); err != nil {
		t.Errorf("Done() after only nil appends = %v, want nil", err)
	}
}

// TestCollector_SingleAppendReturnsErrorVerbatim verifies the single-error
// path returns the error as-is so errors.Is still matches sentinels.
func TestCollector_SingleAppendReturnsErrorVerbatim(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	var c errcollect.Collector
	c.Append(sentinel)
	got := c.Done()
	if got != sentinel {
		t.Errorf("Done() = %v, want sentinel verbatim", got)
	}
}

// TestCollector_MultipleAppendsJoinedAndDiscoverable verifies that with
// more than one error, Done returns errors.Join of them and every
// individual error is still discoverable via errors.Is.
func TestCollector_MultipleAppendsJoinedAndDiscoverable(t *testing.T) {
	t.Parallel()
	a := errors.New("a")
	b := errors.New("b")
	c := errors.New("c")

	var col errcollect.Collector
	col.Append(a)
	col.Append(nil)
	col.Append(b)
	col.Append(c)

	got := col.Done()
	if got == nil {
		t.Fatal("Done() = nil, want joined error")
	}
	for _, want := range []error{a, b, c} {
		if !errors.Is(got, want) {
			t.Errorf("errors.Is(%v, %v) = false, want true", got, want)
		}
	}
}
