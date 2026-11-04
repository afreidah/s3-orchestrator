// -------------------------------------------------------------------------------
// Login Throttle - Per-IP Brute-Force Protection
//
// Author: Alex Freidah
//
// Per-IP login attempt tracker with automatic lockout after repeated failures.
// After MaxFailures consecutive bad attempts from an IP, that IP is locked out
// for LockoutDuration. Successful login resets the counter. Entries whose
// lockout has passed are swept in the background.
// -------------------------------------------------------------------------------

package httputil

import (
	"time"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// LoginThrottle tracks failed login attempts per IP and enforces lockout.
type LoginThrottle struct {
	attempts        *ipTable[loginAttempt]
	maxFailures     int
	lockoutDuration time.Duration
}

// loginAttempt is the per-IP entry the throttle keeps in memory:
// running failure count and the timestamp at which the IP becomes
// eligible to retry. Resets to a fresh zero value once a login
// succeeds so a one-off typo doesn't permanently degrade an IP.
type loginAttempt struct {
	failures    int
	lockedUntil time.Time
}

// loginThrottleSweep is how often stale per-IP entries are removed.
const loginThrottleSweep = 5 * time.Minute

// NewLoginThrottle creates a login throttle with the given limits.
func NewLoginThrottle(maxFailures int, lockoutDuration time.Duration) *LoginThrottle {
	return &LoginThrottle{
		attempts: newIPTable(loginThrottleSweep, func(a *loginAttempt, now time.Time) bool {
			return now.After(a.lockedUntil)
		}),
		maxFailures:     maxFailures,
		lockoutDuration: lockoutDuration,
	}
}

// -------------------------------------------------------------------------
// PUBLIC API
// -------------------------------------------------------------------------

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (lt *LoginThrottle) Close() {
	lt.attempts.close()
}

// IsLockedOut returns true if the given IP is currently locked out.
// Callers must pass a resolved client IP (e.g. from ExtractClientIP),
// not a raw remoteAddr.
func (lt *LoginThrottle) IsLockedOut(ip string) bool {
	var locked bool
	lt.attempts.visit(ip, func(a *loginAttempt) *loginAttempt {
		locked = a != nil && time.Now().Before(a.lockedUntil)
		return a
	})
	return locked
}

// RecordFailure increments the failure count for the given IP. If the count
// reaches MaxFailures, the IP is locked out for LockoutDuration.
// Callers must pass a resolved client IP (e.g. from ExtractClientIP),
// not a raw remoteAddr.
func (lt *LoginThrottle) RecordFailure(ip string) {
	lt.attempts.visit(ip, func(a *loginAttempt) *loginAttempt {
		if a == nil {
			a = &loginAttempt{}
		}
		a.failures++
		if a.failures >= lt.maxFailures {
			a.lockedUntil = time.Now().Add(lt.lockoutDuration)
		}
		return a
	})
}

// RecordSuccess resets the failure counter for the given IP.
// Callers must pass a resolved client IP (e.g. from ExtractClientIP),
// not a raw remoteAddr.
func (lt *LoginThrottle) RecordSuccess(ip string) {
	lt.attempts.visit(ip, func(*loginAttempt) *loginAttempt { return nil })
}
