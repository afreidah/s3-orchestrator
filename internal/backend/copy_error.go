// -------------------------------------------------------------------------------
// Backend - Stream Copy Errors
//
// Author: Alex Freidah
//
// Typed errors for the read/write phases of a backend-to-backend stream copy.
// Producers (infra.BackendRuntime.StreamCopy) wrap the underlying backend error
// with a phase tag so consumers (the replicator's per-source retry loop) can
// classify failures structurally - "destination is broken, do not try other
// sources" vs "this source failed, move on to the next" - without inspecting
// rendered error strings.
// -------------------------------------------------------------------------------

package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// CopyEndpoint pairs a backend client with the name its usage is charged to,
// so a stream copy can check egress on one backend and ingress on the other at
// the point the bytes move.
type CopyEndpoint struct {
	Name    string
	Backend ObjectBackend
}

// CopyRequest is one backend-to-backend copy: read SrcKey from Src and write
// DstKey on Dst. SizeEstimate is what both legs are admitted against before
// any byte moves. Transform, when set, rewrites the bytes between the legs;
// nil copies them verbatim.
type CopyRequest struct {
	Src, Dst       CopyEndpoint
	SrcKey, DstKey string
	SizeEstimate   int64
	Transform      CopyTransform
}

// CopyTransform turns the object a copy read into the bytes it writes,
// returning the body, its size and a release for anything it buffered. The
// release runs once the write is done, whatever its outcome.
type CopyTransform func(ctx context.Context, src *GetObjectResult) (body io.Reader, size int64, release func(), err error)

// CopyPhase identifies which leg of a stream copy failed.
type CopyPhase string

// CopyPhase values. Read failures are recoverable by trying another
// source; write failures terminate the attempt because the destination
// rejected the data. A transform failure is about the bytes read, and the
// transform's own error says why.
const (
	CopyPhaseRead      CopyPhase = "read"
	CopyPhaseTransform CopyPhase = "transform"
	CopyPhaseWrite     CopyPhase = "write"
)

// CopyError tags a stream-copy failure with the phase that produced it
// and preserves the underlying error for errors.Is / errors.As walks.
// Error renders as "<phase>: <underlying>", so a log line names the phase
// before the cause.
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
