// -------------------------------------------------------------------------------
// Core Errors - S3Error Behavior Tests
//
// Author: Alex Freidah
//
// Verifies S3Error.Error returns the human-readable Message field so the
// server layer's structured error path (status + S3 code) can wrap or
// surface the message through standard error handling.
// -------------------------------------------------------------------------------

package core

import (
	"errors"
	"testing"
)

// -------------------------------------------------------------------------
// S3Error.Error
// -------------------------------------------------------------------------

// TestS3Error_ErrorReturnsMessage verifies the Error method returns the
// Message field rather than a synthesised string, so error wrapping
// preserves the original caller-facing text.
func TestS3Error_ErrorReturnsMessage(t *testing.T) {
	t.Parallel()
	e := &S3Error{StatusCode: 404, Code: "NoSuchKey", Message: "object not found"}
	if got := e.Error(); got != "object not found" {
		t.Errorf("Error() = %q, want %q", got, "object not found")
	}
}

// TestS3Error_ErrorOnZeroValue verifies the zero S3Error value returns
// an empty string rather than panicking.
func TestS3Error_ErrorOnZeroValue(t *testing.T) {
	t.Parallel()
	e := &S3Error{}
	if got := e.Error(); got != "" {
		t.Errorf("zero-value Error() = %q, want empty string", got)
	}
}

// TestS3Error_IsErrorIface verifies the type satisfies the standard
// error interface so it works with errors.Is and errors.As.
func TestS3Error_IsErrorIface(t *testing.T) {
	t.Parallel()
	var _ error = &S3Error{}
	// Sentinel errors must compare equal to themselves under errors.Is.
	if !errors.Is(ErrObjectNotFound, ErrObjectNotFound) {
		t.Error("ErrObjectNotFound is not equal to itself under errors.Is")
	}
}
