// -------------------------------------------------------------------------------
// Backend Runtime - Per-Operation Backend Timeouts
//
// Author: Alex Freidah
//
// Applies the configured per-backend-operation timeout to a context
// (honouring a tighter parent deadline) and hosts the composite operations
// that pair it with a backend RPC, so the timeout plumbing lives in one place
// instead of at every backend call site.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// WithTimeout returns a context with the configured backend timeout
// applied. Honours a tighter parent deadline. Returns context.WithCancel
// when no timeout is configured so the caller can always defer the
// cancel without branching on timeout-vs-no-timeout.
func (c *BackendRuntime) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.backendTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < c.backendTimeout {
			return context.WithTimeout(ctx, remaining)
		}
	}
	return context.WithTimeout(ctx, c.backendTimeout)
}

// DeleteWithTimeout deletes the object at storageKey on a backend using the
// configured backend timeout. storageKey is the path the bytes occupy, which
// for a per-write copy is not the object's key.
func (c *BackendRuntime) DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, storageKey string) error {
	dctx, dcancel := c.WithTimeout(ctx)
	defer dcancel()
	return be.DeleteObject(dctx, storageKey)
}

// GetWithTimeout issues a GET against be with the configured backend timeout.
// On success it returns the cancel func instead of deferring it, because the
// caller owns the body: defer it next to Body.Close, or pass it to
// ioutilx.WithCancel when streaming. On error the context is already released.
func (c *BackendRuntime) GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error) {
	gctx, gcancel := c.WithTimeout(ctx)
	result, err := be.GetObject(gctx, key, rangeHeader)
	if err != nil {
		gcancel()
		return nil, nil, err
	}
	return result, gcancel, nil
}

// HeadWithTimeout issues a HEAD against be with the configured backend timeout.
// HEAD carries no body, so the timeout context is fully released before
// returning, mirroring DeleteWithTimeout.
func (c *BackendRuntime) HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error) {
	hctx, hcancel := c.WithTimeout(ctx)
	defer hcancel()
	return be.HeadObject(hctx, key)
}

// StreamCopy reads an object from src and writes it to dst with timeouts
// applied to each leg, admitting the transfer against both backends' usage
// limits first. Returns the bytes moved, or a *backend.CopyError tagged with
// the failing phase.
//
// Admission happens here so every backend-to-backend copy enforces the same
// limits, judged on sizeEstimate. Accounting stays with the caller, which
// charges the size its metadata commit settled on.
//
// srcKey and dstKey differ: the copy is a new write with its own path on the
// destination, so a cleanup after it deletes only those bytes.
func (c *BackendRuntime) StreamCopy(ctx context.Context, src, dst backend.CopyEndpoint, srcKey, dstKey string, sizeEstimate int64) (int64, error) {
	// A refusal is tagged with the leg that had no headroom: another source
	// may have egress left, but a destination that is full ends the attempt.
	if !c.Acct().Allow(src.Name, []s3op.Operation{s3op.GetObject}, sizeEstimate, 0) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseRead,
			Err:   fmt.Errorf("source %s: %w", src.Name, core.ErrUsageLimitExceeded),
		}
	}
	if !c.Acct().Allow(dst.Name, []s3op.Operation{s3op.PutObject}, 0, sizeEstimate) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseWrite,
			Err:   fmt.Errorf("destination %s: %w", dst.Name, core.ErrUsageLimitExceeded),
		}
	}

	rctx, rcancel := c.WithTimeout(ctx)
	defer rcancel()
	result, err := src.Backend.GetObject(rctx, srcKey, "")
	if err != nil {
		return 0, &backend.CopyError{Phase: backend.CopyPhaseRead, Err: err}
	}
	defer func() { _ = result.Body.Close() }()

	// PutObject consumes the source body live, so a write-leg error can come
	// from a failing source read. Attributing it to the read phase keeps a
	// degraded source from tripping a healthy target's breaker.
	wctx, wcancel := c.WithTimeout(ctx)
	defer wcancel()
	tracked := &readTracker{r: result.Body}
	_, err = dst.Backend.PutObject(wctx, dstKey, tracked, result.Size, result.ContentType, result.Metadata)
	if err != nil {
		phase := backend.CopyPhaseWrite
		if tracked.readErr != nil {
			phase = backend.CopyPhaseRead
		}
		return 0, &backend.CopyError{Phase: phase, Err: err}
	}
	return result.Size, nil
}

// readTracker wraps the source body so StreamCopy can tell whether a copy
// failure originated in the source read. It records the first non-EOF read
// error; the destination's PutObject reads through it.
type readTracker struct {
	r       io.Reader
	readErr error
}

// Read proxies to the wrapped reader, capturing the first real read error
// (io.EOF is the normal end-of-stream signal, not a failure).
func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && err != io.EOF && t.readErr == nil {
		t.readErr = err
	}
	return n, err
}
