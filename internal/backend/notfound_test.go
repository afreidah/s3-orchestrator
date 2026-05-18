// -------------------------------------------------------------------------------
// IsNotFound - Tests
//
// Author: Alex Freidah
// -------------------------------------------------------------------------------

package backend

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNotFound_404(t *testing.T) {
	t.Parallel()
	if !IsNotFound(&httpError{code: 404, msg: "NoSuchKey"}) {
		t.Error("expected true for 404")
	}
}

func TestIsNotFound_500(t *testing.T) {
	t.Parallel()
	if IsNotFound(&httpError{code: 500, msg: "InternalServerError"}) {
		t.Error("expected false for 500")
	}
}

func TestIsNotFound_PlainError(t *testing.T) {
	t.Parallel()
	if IsNotFound(errors.New("connection refused")) {
		t.Error("expected false for plain error")
	}
}

func TestIsNotFound_Wrapped404(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("read: %w", &httpError{code: 404, msg: "NoSuchKey"})
	if !IsNotFound(wrapped) {
		t.Error("expected true for wrapped 404")
	}
}

func TestIsNotFound_Nil(t *testing.T) {
	t.Parallel()
	if IsNotFound(nil) {
		t.Error("expected false for nil")
	}
}
