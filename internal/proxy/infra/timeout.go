// -------------------------------------------------------------------------------
// Timeout Policy - Per-Operation Backend Timeouts
//
// Author: Alex Freidah
//
// Owns the configured per-backend-operation timeout duration and the
// helpers that apply it to a context (honouring a tighter parent
// deadline) before issuing a backend call. Also hosts the small
// composite operations that pair a timeout with a single backend RPC
// (DeleteWithTimeout, StreamCopy) so the timeout-plumbing pattern lives
// in one place instead of being duplicated at every backend call site.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"io"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
)

// timeoutPolicy owns the per-backend-operation timeout. A value of 0
// disables the timeout (callers get the parent context unchanged
// modulo a wrapping cancel for symmetric defer cleanup).
type timeoutPolicy struct {
	backendTimeout time.Duration
}

// newTimeoutPolicy constructs the policy with the configured per-call
// backend timeout.
func newTimeoutPolicy(backendTimeout time.Duration) *timeoutPolicy {
	return &timeoutPolicy{backendTimeout: backendTimeout}
}

// WithTimeout returns a context with the configured backend timeout
// applied. Honours a tighter parent deadline. Returns context.WithCancel
// when no timeout is configured so the caller can always defer the
// cancel without branching on timeout-vs-no-timeout.
func (p *timeoutPolicy) WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if p.backendTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < p.backendTimeout {
			return context.WithTimeout(ctx, remaining)
		}
	}
	return context.WithTimeout(ctx, p.backendTimeout)
}

// DeleteWithTimeout deletes an object from a backend using the
// configured backend timeout.
func (p *timeoutPolicy) DeleteWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) error {
	dctx, dcancel := p.WithTimeout(ctx)
	defer dcancel()
	return be.DeleteObject(dctx, key)
}

// GetWithTimeout issues a GET against be with the configured backend timeout.
// GET returns a streaming body whose lifetime the caller owns, so on success
// the cancel func is handed back instead of being deferred: synchronous readers
// defer it next to Body.Close, while streaming callers pass it to
// ioutilx.WithCancel so the timeout context is released when the body is closed.
// On error there is no body to own, so the context is released before returning.
func (p *timeoutPolicy) GetWithTimeout(ctx context.Context, be backend.ObjectBackend, key, rangeHeader string) (*backend.GetObjectResult, context.CancelFunc, error) {
	gctx, gcancel := p.WithTimeout(ctx)
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
func (p *timeoutPolicy) HeadWithTimeout(ctx context.Context, be backend.ObjectBackend, key string) (*backend.HeadObjectResult, error) {
	hctx, hcancel := p.WithTimeout(ctx)
	defer hcancel()
	return be.HeadObject(hctx, key)
}

// StreamCopy reads an object from src and writes it to dst with the
// configured backend timeout applied to each leg. Returns a
// *backend.CopyError tagged with the failing phase so callers can
// distinguish read-side from write-side failures.
//
// PutObject consumes the source body live, so a write-leg error can
// actually originate in a slow or failing source read. The body is wrapped
// in a readTracker: if the read errored, the failure is attributed to the
// read phase (retry another source) rather than the write phase (blame the
// target), preventing a degraded source from tripping a healthy target's
// breaker.
func (p *timeoutPolicy) StreamCopy(ctx context.Context, src, dst backend.ObjectBackend, key string) error {
	rctx, rcancel := p.WithTimeout(ctx)
	defer rcancel()
	result, err := src.GetObject(rctx, key, "")
	if err != nil {
		return &backend.CopyError{Phase: backend.CopyPhaseRead, Err: err}
	}
	defer func() { _ = result.Body.Close() }()

	wctx, wcancel := p.WithTimeout(ctx)
	defer wcancel()
	tracked := &readTracker{r: result.Body}
	_, err = dst.PutObject(wctx, key, tracked, result.Size, result.ContentType, result.Metadata)
	if err != nil {
		phase := backend.CopyPhaseWrite
		if tracked.readErr != nil {
			phase = backend.CopyPhaseRead
		}
		return &backend.CopyError{Phase: phase, Err: err}
	}
	return nil
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
