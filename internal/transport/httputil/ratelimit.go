// -------------------------------------------------------------------------------
// Rate Limiter - Per-IP Token Bucket Throttling
//
// Author: Alex Freidah
//
// Per-IP token bucket rate limiter with automatic cleanup of stale entries.
// When enabled, requests exceeding the configured rate receive a 429 SlowDown
// in the error format of the surface they reached.
// Supports X-Forwarded-For for accurate IP extraction behind trusted reverse
// proxies. When trusted_proxies is configured, only requests arriving from a
// trusted proxy CIDR will have their X-Forwarded-For header inspected; all
// other requests use RemoteAddr directly. The rightmost non-trusted IP in the
// XFF chain is used as the client IP, which is the standard secure extraction
// method (the rightmost entry appended by your trusted proxy).
// -------------------------------------------------------------------------------

package httputil

import (
	"cmp"
	"net"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/logfmt"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RateLimiter provides per-IP token-bucket rate limiting. rate and burst are
// read and changed under the visitor table's lock.
type RateLimiter struct {
	visitors       *ipTable[visitorLimiter]
	rate           rate.Limit
	burst          int
	trustedProxies []*net.IPNet
}

// visitorLimiter is the per-IP token bucket plus the lastSeen
// timestamp used by the eviction sweep. lastSeen is updated on every
// allow check so an IP that goes idle gets reaped after
// cleanupMaxAge, freeing the map entry without surprising an active
// caller.
type visitorLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// -------------------------------------------------------------------------
// CONSTRUCTOR AND CONFIG
// -------------------------------------------------------------------------

// NewRateLimiter creates a rate limiter with the given configuration, whose
// defaults the config loader filled.
func NewRateLimiter(cfg config.RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		visitors: newIPTable(cfg.CleanupInterval, func(v *visitorLimiter, now time.Time) bool {
			return v.lastSeen.Before(now.Add(-cfg.CleanupMaxAge))
		}),
		rate:           rate.Limit(cfg.RequestsPerSec),
		burst:          cfg.Burst,
		trustedProxies: ParseTrustedProxies(cfg.TrustedProxies),
	}
}

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (rl *RateLimiter) Close() {
	rl.visitors.close()
}

// UpdateLimits changes the rate and burst and resets all existing per-IP
// limiters so the new limits take effect immediately.
func (rl *RateLimiter) UpdateLimits(requestsPerSec float64, burst int) {
	rl.visitors.reset(func() {
		rl.rate = rate.Limit(requestsPerSec)
		rl.burst = burst
	})
}

// -------------------------------------------------------------------------
// ALLOW
// -------------------------------------------------------------------------

// Allow checks whether a request from the given IP is allowed.
func (rl *RateLimiter) Allow(ip string) bool {
	var limiter *rate.Limiter
	rl.visitors.visit(ip, func(v *visitorLimiter) *visitorLimiter {
		if v == nil {
			v = &visitorLimiter{limiter: rate.NewLimiter(rl.rate, rl.burst)}
		}
		v.lastSeen = time.Now()
		limiter = v.limiter
		return v
	})
	return limiter.Allow()
}

// -------------------------------------------------------------------------
// MIDDLEWARE
// -------------------------------------------------------------------------

// Middleware returns per-IP rate limiting for one surface. A refused request
// is answered through writeErr, in the format that surface's clients read, and
// audited as route.RateLimitRejected, where route is "s3" or "admin".
func (rl *RateLimiter) Middleware(route string, writeErr ErrorWriter) func(http.Handler) http.Handler {
	event := route + ".RateLimitRejected"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ExtractClientIP(r, rl.trustedProxies)
			if !rl.Allow(ip) {
				telemetry.RateLimitRejectionsTotal.Inc()
				RefuseThrottled(w, r, writeErr, &Throttled{
					Event: event, Status: http.StatusTooManyRequests, Message: "Rate limit exceeded", Client: ip,
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// -------------------------------------------------------------------------
// THROTTLED REFUSAL
// -------------------------------------------------------------------------

// Throttled is a request turned away for load before it reached its handler:
// the audit event it is filed under, the status and message it answers with,
// and the client address it names, r.RemoteAddr when empty.
type Throttled struct {
	Event   string
	Status  int
	Message string
	Client  string
}

// RefuseThrottled audits t and answers SlowDown through writeErr, asking the
// client to retry in a second. Every load refusal on every surface answers and
// records the same way.
func RefuseThrottled(w http.ResponseWriter, r *http.Request, writeErr ErrorWriter, t *Throttled) {
	audit.Log(r.Context(), t.Event, logfmt.RequestFrom(r, cmp.Or(t.Client, r.RemoteAddr))...)
	w.Header().Set("Retry-After", "1")
	writeErr(w, t.Status, "SlowDown", t.Message)
}
