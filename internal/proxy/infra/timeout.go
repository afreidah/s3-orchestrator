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
// returning.
func (c *BackendRuntime) HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error) {
	hctx, hcancel := c.WithTimeout(ctx)
	defer hcancel()
	return be.HeadObject(hctx, key)
}

// StreamCopy reads an object from the request's source and writes it to its
// destination with timeouts applied to each leg, admitting the transfer
// against both backends' usage limits first and charging what each leg moved.
// Returns the bytes written, or a *backend.CopyError tagged with the failing
// phase.
//
// Admission and accounting happen here so every backend-to-backend copy
// enforces the same limits, judged on the request's size estimate, and is
// charged the same way.
//
// The two keys differ: the copy is a new write with its own path on the
// destination, so a cleanup after it deletes only those bytes.
func (c *BackendRuntime) StreamCopy(ctx context.Context, req *backend.CopyRequest) (int64, error) {
	src, dst := req.Src, req.Dst
	// A refusal is tagged with the leg that had no headroom: another source
	// may have egress left, but a destination that is full ends the attempt.
	if !c.Acct().Allow(src.Name, []s3op.Operation{s3op.GetObject}, req.SizeEstimate, 0) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseRead,
			Err:   fmt.Errorf("source %s: %w", src.Name, core.ErrUsageLimitExceeded),
		}
	}
	if !c.Acct().Allow(dst.Name, []s3op.Operation{s3op.PutObject}, 0, req.SizeEstimate) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseWrite,
			Err:   fmt.Errorf("destination %s: %w", dst.Name, core.ErrUsageLimitExceeded),
		}
	}

	rctx, rcancel := c.WithTimeout(ctx)
	defer rcancel()
	result, err := src.Backend.GetObject(rctx, req.SrcKey, "")
	if err != nil {
		c.Acct().APICall(s3op.GetObject, src.Name)
		return 0, &backend.CopyError{Phase: backend.CopyPhaseRead, Err: err}
	}
	defer func() { _ = result.Body.Close() }()
	c.Acct().Egress(s3op.GetObject, src.Name, result.Size)

	// PutObject consumes the source body live, so a write-leg error can come
	// from a failing source read. Attributing it to the read phase keeps a
	// degraded source from tripping a healthy target's breaker.
	tracked := &readTracker{r: result.Body}
	body, size, release, err := copyBody(ctx, req.Transform, result, tracked)
	if err != nil {
		return 0, &backend.CopyError{Phase: backend.CopyPhaseTransform, Err: err}
	}
	defer release()
	wctx, wcancel := c.WithTimeout(ctx)
	defer wcancel()
	_, err = dst.Backend.PutObject(wctx, req.DstKey, body, size, result.ContentType, result.Metadata)
	if err != nil {
		c.Acct().APICall(s3op.PutObject, dst.Name)
		phase := backend.CopyPhaseWrite
		if tracked.readErr != nil {
			phase = backend.CopyPhaseRead
		}
		return 0, &backend.CopyError{Phase: phase, Err: err}
	}
	c.Acct().Ingress(s3op.PutObject, dst.Name, size)
	return size, nil
}

// copyBody is what a copy writes: the tracked source as read, or what the
// transform made of it. The release is always safe to call.
func copyBody(ctx context.Context, transform backend.CopyTransform, result *backend.GetObjectResult, tracked *readTracker) (io.Reader, int64, func(), error) {
	if transform == nil {
		return tracked, result.Size, func() {
			// Nothing was buffered.
		}, nil
	}
	read := *result
	read.Body = io.NopCloser(tracked)
	body, size, release, err := transform(ctx, &read)
	if err != nil {
		return nil, 0, nil, err
	}
	if release == nil {
		release = func() {
			// The transform streams and buffered nothing.
		}
	}
	return body, size, release, nil
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
