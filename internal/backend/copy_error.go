// -------------------------------------------------------------------------------
// Backend - Stream Copy Errors
//
// Author: Alex Freidah
//
// Typed errors for the read/write phases of a backend-to-backend stream copy.
// Producers (proxy.backendCore.StreamCopy) wrap the underlying backend error
// with a phase tag so consumers (the replicator's per-source retry loop) can
// classify failures structurally - "destination is broken, do not try other
// sources" vs "this source failed, move on to the next" - without inspecting
// rendered error strings.
// -------------------------------------------------------------------------------

package backend

import (
	"errors"
	"fmt"
)

// CopyPhase identifies which leg of a stream copy failed.
type CopyPhase string

// CopyPhase values. Read failures are recoverable by trying another
// source; write failures terminate the attempt because the destination
// rejected the data.
const (
	CopyPhaseRead  CopyPhase = "read"
	CopyPhaseWrite CopyPhase = "write"
)

// CopyError tags a stream-copy failure with the phase that produced it
// and preserves the underlying error for errors.Is / errors.As walks.
// Error renders as "<phase>: <underlying>" so log output matches the
// historical string-prefix shape.
type CopyError struct {
	Phase CopyPhase
	Err   error
}

// Error renders the wrapped failure with the phase prefix.
func (e *CopyError) Error() string {
	return fmt.Sprintf("%s: %s", e.Phase, e.Err.Error())
}

// Unwrap exposes the wrapped error to errors.Is / errors.As callers.
func (e *CopyError) Unwrap() error {
	return e.Err
}

// IsCopyPhase reports whether err is (or wraps) a CopyError whose
// Phase matches phase. Returns false on nil err or unrelated errors.
func IsCopyPhase(err error, phase CopyPhase) bool {
	ce, ok := errors.AsType[*CopyError](err)
	if !ok {
		return false
	}
	return ce.Phase == phase
}
