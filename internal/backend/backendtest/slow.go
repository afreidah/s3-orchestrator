// -------------------------------------------------------------------------------
// Backendtest - Latency Decorator
//
// Author: Alex Freidah
//
// Wraps any ObjectBackend and delays the chosen operations, so a test can
// exercise the caller's timeout and concurrency behaviour without a real slow
// backend. The delay is cancellable: an operation whose context expires first
// returns the context error rather than sleeping it out, which is what makes
// the caller's own timeout observable.
// -------------------------------------------------------------------------------

package backendtest

import (
	"context"
	"io"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/backend"
)

// Slow delays the operations selected by its Delay* fields. Construct with
// NewSlow; the embedded backend serves every operation not delayed.
type Slow struct {
	backend.ObjectBackend

	delay time.Duration

	// DelayPuts, DelayGets, and DelayHeads select which operations wait.
	// Puts are delayed by default because that is the common case; the
	// read side stays fast unless a test asks otherwise.
	DelayPuts  bool
	DelayGets  bool
	DelayHeads bool
}

// NewSlow wraps inner so PutObject waits for d. Set DelayGets or DelayHeads on
// the result to slow the read side too.
func NewSlow(inner backend.ObjectBackend, d time.Duration) *Slow {
	return &Slow{ObjectBackend: inner, delay: d, DelayPuts: true}
}

// wait sleeps for the configured delay unless ctx expires first, in which case
// it reports the context error so the caller's timeout is what surfaces.
func (s *Slow) wait(ctx context.Context, enabled bool) error {
	if !enabled {
		return nil
	}
	select {
	case <-time.After(s.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PutObject delays when DelayPuts is set, then forwards.
func (s *Slow) PutObject(
	ctx context.Context, key string, body io.Reader, size int64, contentType string, metadata map[string]string,
) (string, error) {
	if err := s.wait(ctx, s.DelayPuts); err != nil {
		return "", err
	}
	return s.ObjectBackend.PutObject(ctx, key, body, size, contentType, metadata)
}

// GetObject delays when DelayGets is set, then forwards.
func (s *Slow) GetObject(ctx context.Context, key, rangeHeader string) (*backend.GetObjectResult, error) {
	if err := s.wait(ctx, s.DelayGets); err != nil {
		return nil, err
	}
	return s.ObjectBackend.GetObject(ctx, key, rangeHeader)
}

// HeadObject delays when DelayHeads is set, then forwards.
func (s *Slow) HeadObject(ctx context.Context, key string) (*backend.HeadObjectResult, error) {
	if err := s.wait(ctx, s.DelayHeads); err != nil {
		return nil, err
	}
	return s.ObjectBackend.HeadObject(ctx, key)
}
