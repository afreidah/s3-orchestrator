// -------------------------------------------------------------------------------
// Fake Advisory Locker
//
// Author: Alex Freidah
//
// Stands in for the metadata store's advisory lock in tests of the services
// that run under one, so each test package does not grow its own fakes.
// -------------------------------------------------------------------------------

package testx

import "context"

// Locker is a fake advisory locker. Err fails the lock call, Busy reports the
// lock held by another instance, and otherwise fn runs under the lock.
type Locker struct {
	Busy bool
	Err  error
}

// WithAdvisoryLock reports whether the lock was taken and returns fn's error
// when it ran.
func (l Locker) WithAdvisoryLock(ctx context.Context, _ int64, fn func(ctx context.Context) error) (bool, error) {
	if l.Err != nil {
		return false, l.Err
	}
	if l.Busy {
		return false, nil
	}
	return true, fn(ctx)
}
