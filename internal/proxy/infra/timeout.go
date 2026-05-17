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

// StreamCopy reads an object from src and writes it to dst with the
// configured backend timeout applied to each leg. Returns a
// *backend.CopyError tagged with the failing phase so callers can
// distinguish read-side from write-side failures.
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
	_, err = dst.PutObject(wctx, key, result.Body, result.Size, result.ContentType, result.Metadata)
	if err != nil {
		return &backend.CopyError{Phase: backend.CopyPhaseWrite, Err: err}
	}
	return nil
}
