// -------------------------------------------------------------------------------
// Login Throttle - Per-IP Brute-Force Protection
//
// Author: Alex Freidah
//
// Per-IP login attempt tracker with automatic lockout after repeated failures.
// After MaxFailures consecutive bad attempts from an IP, that IP is locked out
// for LockoutDuration. Successful login resets the counter. Stale entries are
// cleaned up by a background goroutine.
// -------------------------------------------------------------------------------

package httputil

import (
	"sync"
	"time"
)

// LoginThrottle tracks failed login attempts per IP and enforces lockout.
type LoginThrottle struct {
	mu              sync.Mutex
	attempts        map[string]*loginAttempt
	maxFailures     int
	lockoutDuration time.Duration
	stop            chan struct{}
	closeOnce       sync.Once
}

type loginAttempt struct {
	failures    int
	lockedUntil time.Time
}

// NewLoginThrottle creates a login throttle with the given limits.
func NewLoginThrottle(maxFailures int, lockoutDuration time.Duration) *LoginThrottle {
	lt := &LoginThrottle{
		attempts:        make(map[string]*loginAttempt),
		maxFailures:     maxFailures,
		lockoutDuration: lockoutDuration,
		stop:            make(chan struct{}),
	}

	// Background cleanup of stale entries every 5 minutes
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				lt.cleanup()
			case <-lt.stop:
				return
			}
		}
	}()

	return lt
}

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (lt *LoginThrottle) Close() {
	lt.closeOnce.Do(func() {
		close(lt.stop)
	})
}

// IsLockedOut returns true if the given IP is currently locked out.
// Callers must pass a resolved client IP (e.g. from ExtractClientIP),
// not a raw remoteAddr.
func (lt *LoginThrottle) IsLockedOut(ip string) bool {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	a, ok := lt.attempts[ip]
	if !ok {
		return false
	}
	return time.Now().Before(a.lockedUntil)
}

// RecordFailure increments the failure count for the given IP. If the count
// reaches MaxFailures, the IP is locked out for LockoutDuration.
// Callers must pass a resolved client IP (e.g. from ExtractClientIP),
// not a raw remoteAddr.
func (lt *LoginThrottle) RecordFailure(ip string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()

	a, ok := lt.attempts[ip]
	if !ok {
		a = &loginAttempt{}
		lt.attempts[ip] = a
	}
	a.failures++
	if a.failures >= lt.maxFailures {
		a.lockedUntil = time.Now().Add(lt.lockoutDuration)
	}
}

// RecordSuccess resets the failure counter for the given IP.
// Callers must pass a resolved client IP (e.g. from ExtractClientIP),
// not a raw remoteAddr.
func (lt *LoginThrottle) RecordSuccess(ip string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	delete(lt.attempts, ip)
}

// cleanup removes entries whose lockout has expired.
func (lt *LoginThrottle) cleanup() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	now := time.Now()
	for ip, a := range lt.attempts {
		if now.After(a.lockedUntil) {
			delete(lt.attempts, ip)
		}
	}
}
