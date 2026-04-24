// -------------------------------------------------------------------------------
// Pass-through — AdvisoryLocker
//
// Author: Alex Freidah
//
// Advisory locks are coordination-only; if the database is unreachable the
// lock attempt simply fails and the caller skips the tick. Applying the
// circuit breaker would hide those failures behind ErrDBUnavailable and
// potentially starve background services during probe windows, so this
// wrapper forwards directly without CB protection.
// -------------------------------------------------------------------------------

package store

import (
	"context"
)

// advisoryLockerPassthrough forwards AdvisoryLocker calls directly. It exists
// so DI can hand out an AdvisoryLocker view of the concrete store without
// exposing the full type.
type advisoryLockerPassthrough struct {
	inner AdvisoryLocker
}

// NewAdvisoryLocker returns a direct pass-through view typed as AdvisoryLocker.
func NewAdvisoryLocker(inner AdvisoryLocker) AdvisoryLocker {
	return &advisoryLockerPassthrough{inner: inner}
}

// WithAdvisoryLock forwards without circuit-breaker wrapping.
func (p *advisoryLockerPassthrough) WithAdvisoryLock(ctx context.Context, lockID int64, fn func(ctx context.Context) error) (bool, error) {
	return p.inner.WithAdvisoryLock(ctx, lockID, fn)
}