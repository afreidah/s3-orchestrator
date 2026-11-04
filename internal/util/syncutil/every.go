// -------------------------------------------------------------------------------
// Every - The Periodic Loop
//
// Author: Alex Freidah
//
// The one loop that calls a function on an interval until its context ends,
// and the one wait that gives up when its context does. The wait starts when
// a call returns, so a call that runs longer than the interval is never
// followed straight away by another.
// -------------------------------------------------------------------------------

package syncutil

import (
	"context"
	"math/rand/v2"
	"time"
)

// Schedule says when a loop calls its function. Next, when set, is called
// before every wait and replaces Interval, for a loop whose period changes.
// Jitter adds a random wait of up to half the interval before the first call,
// so instances started together do not all call at once. Immediately makes
// one call before any wait.
type Schedule struct {
	Interval    time.Duration
	Next        func() time.Duration
	Jitter      bool
	Immediately bool
}

// Every calls fn every interval until ctx is done, and returns when it is.
func Every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	Schedule{Interval: interval}.Run(ctx, fn)
}

// Run calls fn on the schedule until ctx is done, and returns when it is. A
// call already running when ctx ends is left to finish.
func (s Schedule) Run(ctx context.Context, fn func(context.Context)) {
	if s.Immediately && ctx.Err() == nil {
		fn(ctx)
	}
	wait := s.wait()
	if half := wait / 2; s.Jitter && half > 0 {
		wait += rand.N(half) //nolint:gosec // G404: spreading start times does not need crypto-strength randomness
	}
	for Sleep(ctx, wait) {
		fn(ctx)
		wait = s.wait()
	}
}

// Sleep waits for d or until ctx is done, and reports whether the wait ran its
// full length, so a caller stops as soon as its context ends.
func Sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// wait returns the next wait: Next's answer when it is set, Interval
// otherwise.
func (s Schedule) wait() time.Duration {
	if s.Next != nil {
		return s.Next()
	}
	return s.Interval
}
