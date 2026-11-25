// -------------------------------------------------------------------------------
// Backend Runtime - Timed and Charged Backend Calls
//
// Author: Alex Freidah
//
// Applies the configured per-backend-operation timeout to a context
// (honouring a tighter parent deadline) and hosts the GET, HEAD, PUT and the
// legs of a copy that pair it with a backend RPC. Each charges the call where it
// is sent, so no call site carries the timeout plumbing or the accounting rule.
// GET and HEAD are also timed for the backend's pressure; a PUT's time depends
// on its size, so it is not.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
	"github.com/afreidah/s3-orchestrator/internal/s3op"
	"github.com/afreidah/s3-orchestrator/internal/store/core"
)

// errBackendTimeout is the cause the configured backend timeout ends a call
// with, which tells it apart from the caller's own deadline or cancellation.
var errBackendTimeout = errors.New("backend timeout")

// WithTimeout returns a context with the configured backend timeout
// applied. Honours a tighter parent deadline. Returns context.WithCancel
// when no timeout is configured so the caller can always defer the
// cancel without branching on timeout-vs-no-timeout.
func (c *BackendRuntime) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cfg.BackendTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < c.cfg.BackendTimeout {
			return context.WithTimeout(ctx, remaining)
		}
	}
	return context.WithTimeoutCause(ctx, c.cfg.BackendTimeout, errBackendTimeout)
}

// timed records, for the backend's pressure, how long a call to name that
// began at start took on ctx, with timedOut set when the backend timeout ended
// it. A call the caller ended says nothing about the backend and is left out.
func (c *BackendRuntime) timed(ctx context.Context, name string, start time.Time, timedOut bool) {
	if ctx.Err() != nil && !timedOut {
		return
	}
	c.Acct().Latency(name, time.Since(start), timedOut)
}

// endedByTimeout reports whether WithTimeout's backend timeout, rather than
// the caller, ended ctx.
func endedByTimeout(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errBackendTimeout)
}

// Get issues a GET against the named backend under the configured timeout and
// charges a call that fails. A successful GET's egress is the caller's to
// charge, since only the reader knows how many bytes it took. On success it
// returns the cancel func instead of deferring it, because the caller owns the
// body: defer it next to Body.Close, or pass it to ioutilx.WithCancel when
// streaming. On error the context is already released.
func (c *BackendRuntime) Get(ctx context.Context, name string, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error) {
	gctx, gcancel := c.WithTimeout(ctx)
	start := time.Now()
	result, err := be.GetObject(gctx, key, rangeHeader)
	c.timed(gctx, name, start, endedByTimeout(gctx))
	if err != nil {
		gcancel()
		c.Acct().APICall(ctx, s3op.GetObject, name)
		return nil, nil, err
	}
	return result, gcancel, nil
}

// OpenGet issues a GET for a body the caller reads later. The timeout covers
// the wait for the response, and start gives the body a full timeout from when
// the caller begins reading it. release frees the call as Get's cancel does,
// and the timer stops with the call however it ends. Only the caller and the
// timer end the call's context, so a call ended while the caller's context is
// still live was ended by the timeout.
func (c *BackendRuntime) OpenGet(ctx context.Context, name string, be backend.ObjectBackend, key string) (result *backend.GetObjectResult, start func(), release context.CancelFunc, err error) {
	gctx, gcancel := context.WithCancel(ctx)
	timeout := c.cfg.BackendTimeout
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, gcancel)
		context.AfterFunc(gctx, func() { timer.Stop() })
	}
	began := time.Now()
	result, err = be.GetObject(gctx, key, "")
	c.timed(gctx, name, began, gctx.Err() != nil && ctx.Err() == nil)
	if err != nil {
		gcancel()
		c.Acct().APICall(ctx, s3op.GetObject, name)
		return nil, nil, nil, err
	}
	if timer != nil {
		timer.Stop()
	}
	start = func() {
		if timer != nil {
			timer.Reset(timeout)
		}
	}
	return result, start, gcancel, nil
}

// Head issues a HEAD against the named backend under the configured timeout
// and charges it whatever the outcome. HEAD carries no body, so the timeout
// context is fully released before returning.
func (c *BackendRuntime) Head(ctx context.Context, name string, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error) {
	hctx, hcancel := c.WithTimeout(ctx)
	defer hcancel()
	start := time.Now()
	result, err := be.HeadObject(hctx, key)
	c.timed(hctx, name, start, endedByTimeout(hctx))
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
// Returns the ETag the destination reported, or a *backend.CopyError tagged
// with the failing phase.
//
// Admission and accounting happen in the two legs, ReadCopy and WriteCopy, so
// every backend-to-backend copy enforces the same limits, judged on the source
// row's size, and is charged the same way. Both legs are admitted before the
// read, so a full destination costs no egress.
//
// The two keys differ: the copy is a new write with its own path on the
// destination, so a cleanup after it deletes only those bytes.
func (c *BackendRuntime) StreamCopy(ctx context.Context, req *backend.CopyRequest) (string, error) {
	if err := c.admitSource(req.Src.Name, req.SrcSize); err != nil {
		return "", err
	}
	if err := c.admitDestination(req.Dst.Name, req.SrcSize); err != nil {
		return "", err
	}
	result, rcancel, err := c.openSource(ctx, req.Src, req.SrcKey, req.SrcSize)
	if err != nil {
		return "", err
	}
	defer rcancel()
	defer func() { _ = result.Body.Close() }()

	// PutObject consumes the source body live, so a write-leg error can come
	// from a failing source read. Attributing it to the read phase keeps a
	// degraded source from tripping a healthy target's breaker.
	tracked := &readTracker{r: result.Body}
	etag, err := c.Put(ctx, s3op.PutObject, &backend.PutRequest{
		Dst: req.Dst, Key: req.DstKey, Body: tracked, Size: result.Size,
		ContentType: result.ContentType, Metadata: result.Metadata,
	})
	if err != nil {
		phase := backend.CopyPhaseWrite
		if tracked.readErr != nil {
			phase = backend.CopyPhaseRead
		}
		return "", &backend.CopyError{Phase: phase, Err: err}
	}
	return etag, nil
}

// ReadCopy opens the read leg of a copy: key on src, admitted against the
// source's usage limits on size and charged for its egress. The cancel and the
// body are the caller's, as with Get. A source that reads at any size other
// than the one its row records is damaged, so the read fails rather than
// handing those bytes on. Failures are a *backend.CopyError in the read phase.
func (c *BackendRuntime) ReadCopy(ctx context.Context, src backend.Endpoint, key string, size int64) (*backend.GetObjectResult, context.CancelFunc, error) {
	if err := c.admitSource(src.Name, size); err != nil {
		return nil, nil, err
	}
	return c.openSource(ctx, src, key, size)
}

// WriteCopy sends the write leg of a copy, admitted against the destination's
// usage limits on the size it writes. Failures are a *backend.CopyError in the
// write phase.
func (c *BackendRuntime) WriteCopy(ctx context.Context, req *backend.PutRequest) (string, error) {
	if err := c.admitDestination(req.Dst.Name, req.Size); err != nil {
		return "", err
	}
	etag, err := c.Put(ctx, s3op.PutObject, req)
	if err != nil {
		return "", &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: err}
	}
	return etag, nil
}

// admitSource refuses a copy read of size bytes that the source's egress
// limit has no room for. The refusal is tagged with the read phase, since
// another source may have egress left.
func (c *BackendRuntime) admitSource(name string, size int64) error {
	if c.Acct().Allow(name, s3op.GetObject, size, 0) {
		return nil
	}
	return &backend.CopyError{
		Phase: backend.CopyPhaseRead,
		Err:   fmt.Errorf("source %s: %w", name, core.ErrUsageLimitExceeded),
	}
}

// admitDestination refuses a copy write of size bytes that the destination's
// ingress limit has no room for. The refusal is tagged with the write phase,
// since a full destination ends the attempt whichever source it reads.
func (c *BackendRuntime) admitDestination(name string, size int64) error {
	if c.Acct().Allow(name, s3op.PutObject, 0, size) {
		return nil
	}
	return &backend.CopyError{
		Phase: backend.CopyPhaseWrite,
		Err:   fmt.Errorf("destination %s: %w", name, core.ErrUsageLimitExceeded),
	}
}

// openSource GETs an admitted copy source, charges its egress, and checks it
// read at the size its row records. On error the call is already released.
func (c *BackendRuntime) openSource(ctx context.Context, src backend.Endpoint, key string, size int64) (*backend.GetObjectResult, context.CancelFunc, error) {
	result, cancel, err := c.Get(ctx, src.Name, src.Backend, key, "")
	if err != nil {
		return nil, nil, &backend.CopyError{Phase: backend.CopyPhaseRead, Err: err}
	}
	c.Acct().Egress(ctx, s3op.GetObject, src.Name, result.Size)
	if result.Size != size {
		_ = result.Body.Close()
		cancel()
		return nil, nil, &backend.CopyError{
			Phase: backend.CopyPhaseRead,
			Err:   fmt.Errorf("source %s read %d bytes, row records %d: %w", src.Name, result.Size, size, backend.ErrSourceSizeMismatch),
		}
	}
	return result, cancel, nil
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
