// -------------------------------------------------------------------------------
// Backoff - Attempt-Indexed Exponential Delay
//
// Author: Alex Freidah
//
// The retry queues stamp a row's next attempt from how many it has already
// had. Each declares its base and ceiling once as an Exponential and reads the
// delay from it, so the doubling and the guard against shifting past the
// ceiling live in one place.
// -------------------------------------------------------------------------------

package backoff

import "time"

// Exponential doubles Base once per attempt and stops at Ceiling.
type Exponential struct {
	Base    time.Duration
	Ceiling time.Duration
}

// For returns the delay before the attempt after attempts failed ones:
// Base * 2^attempts, capped at Ceiling. A negative count gets the ceiling,
// like a count large enough to overflow the doubling.
func (e Exponential) For(attempts int32) time.Duration {
	if attempts < 0 {
		return e.Ceiling
	}
	d := e.Base
	for range attempts {
		if d >= e.Ceiling/2 {
			return e.Ceiling
		}
		d *= 2
	}
	return min(d, e.Ceiling)
}
