// -------------------------------------------------------------------------------
// Backend Runtime - Timed and Charged Backend Calls
//
// Author: Alex Freidah
//
// Applies the configured per-backend-operation timeout to a context
// (honouring a tighter parent deadline) and hosts the GET, HEAD, PUT and
// stream copy that pair it with a backend RPC. Each charges the call where it
// is sent, so no call site carries the timeout plumbing or the accounting rule.
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

// Get issues a GET against the named backend under the configured timeout and
// charges a call that fails. A successful GET's egress is the caller's to
// charge, since only the reader knows how many bytes it took. On success it
// returns the cancel func instead of deferring it, because the caller owns the
// body: defer it next to Body.Close, or pass it to ioutilx.WithCancel when
// streaming. On error the context is already released.
func (c *BackendRuntime) Get(ctx context.Context, name string, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error) {
	gctx, gcancel := c.WithTimeout(ctx)
	result, err := be.GetObject(gctx, key, rangeHeader)
	if err != nil {
		gcancel()
		c.Acct().APICall(ctx, s3op.GetObject, name)
		return nil, nil, err
	}
	return result, gcancel, nil
}

// Head issues a HEAD against the named backend under the configured timeout
// and charges it whatever the outcome. HEAD carries no body, so the timeout
// context is fully released before returning.
func (c *BackendRuntime) Head(ctx context.Context, name string, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error) {
	hctx, hcancel := c.WithTimeout(ctx)
	defer hcancel()
	result, err := be.HeadObject(hctx, key)
	c.Acct().APICall(ctx, s3op.HeadObject, name)
	return result, err
}

// Put writes req under the configured timeout and charges it as op: a failed
// call as one API call, a successful one with the ingress of the bytes that
// landed, whether or not the write that sent them is recorded afterwards.
func (c *BackendRuntime) Put(ctx context.Context, op s3op.Operation, req *backend.PutRequest) (string, error) {
	pctx, pcancel := c.WithTimeout(ctx)
	defer pcancel()
	etag, err := req.Dst.Backend.PutObject(pctx, req.Key, req.Body, req.Size, req.ContentType, req.Metadata)
	if err != nil {
		c.Acct().APICall(ctx, op, req.Dst.Name)
		return "", err
	}
	c.Acct().Ingress(ctx, op, req.Dst.Name, req.Size)
	return etag, nil
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
	if !c.Acct().Allow(src.Name, s3op.GetObject, req.SizeEstimate, 0) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseRead,
			Err:   fmt.Errorf("source %s: %w", src.Name, core.ErrUsageLimitExceeded),
		}
	}
	if !c.Acct().Allow(dst.Name, s3op.PutObject, 0, req.SizeEstimate) {
		return 0, &backend.CopyError{
			Phase: backend.CopyPhaseWrite,
			Err:   fmt.Errorf("destination %s: %w", dst.Name, core.ErrUsageLimitExceeded),
		}
	}

	result, rcancel, err := c.Get(ctx, src.Name, src.Backend, req.SrcKey, "")
	if err != nil {
		return 0, &backend.CopyError{Phase: backend.CopyPhaseRead, Err: err}
	}
	defer rcancel()
	defer func() { _ = result.Body.Close() }()
	c.Acct().Egress(ctx, s3op.GetObject, src.Name, result.Size)

	// PutObject consumes the source body live, so a write-leg error can come
	// from a failing source read. Attributing it to the read phase keeps a
	// degraded source from tripping a healthy target's breaker.
	tracked := &readTracker{r: result.Body}
	body, size, release, err := copyBody(ctx, req.Transform, result, tracked)
	if err != nil {
		return 0, &backend.CopyError{Phase: backend.CopyPhaseTransform, Err: err}
	}
	defer release()
	if _, err := c.Put(ctx, s3op.PutObject, &backend.PutRequest{
		Dst: dst, Key: req.DstKey, Body: body, Size: size,
		ContentType: result.ContentType, Metadata: result.Metadata,
	}); err != nil {
		phase := backend.CopyPhaseWrite
		if tracked.readErr != nil {
			phase = backend.CopyPhaseRead
		}
		return 0, &backend.CopyError{Phase: phase, Err: err}
	}
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
