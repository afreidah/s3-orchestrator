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

package s3api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/util/syncutil"
)

// -------------------------------------------------------------------------
// TYPES
// -------------------------------------------------------------------------

// RateLimiter provides per-IP token-bucket rate limiting.
type RateLimiter struct {
	mu             sync.Mutex
	limiters       map[string]*visitorLimiter
	rate           rate.Limit
	burst          int
	trustedProxies []*net.IPNet
	stop           context.CancelFunc
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
	ctx, stop := context.WithCancel(context.Background())
	rl := &RateLimiter{
		limiters:       make(map[string]*visitorLimiter),
		rate:           rate.Limit(cfg.RequestsPerSec),
		burst:          cfg.Burst,
		trustedProxies: httputil.ParseTrustedProxies(cfg.TrustedProxies),
		stop:           stop,
	}
	go syncutil.Every(ctx, cfg.CleanupInterval, func(context.Context) { rl.cleanup(cfg.CleanupMaxAge) })
	return rl
}

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (rl *RateLimiter) Close() {
	rl.stop()
}

// UpdateLimits changes the rate and burst and resets all existing per-IP
// limiters so the new limits take effect immediately.
func (rl *RateLimiter) UpdateLimits(requestsPerSec float64, burst int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.rate = rate.Limit(requestsPerSec)
	rl.burst = burst
	rl.limiters = make(map[string]*visitorLimiter)
}

// -------------------------------------------------------------------------
// ALLOW / CLEANUP
// -------------------------------------------------------------------------

// Allow checks whether a request from the given IP is allowed.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	v, ok := rl.limiters[ip]
	if !ok {
		v = &visitorLimiter{
			limiter: rate.NewLimiter(rl.rate, rl.burst),
		}
		rl.limiters[ip] = v
	}
	v.lastSeen = time.Now()
	rl.mu.Unlock()

	return v.limiter.Allow()
}

// cleanup removes entries not seen within the given duration.
func (rl *RateLimiter) cleanup(maxAge time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for ip, v := range rl.limiters {
		if v.lastSeen.Before(cutoff) {
			delete(rl.limiters, ip)
		}
	}
}

// -------------------------------------------------------------------------
// MIDDLEWARE AND IP EXTRACTION
// -------------------------------------------------------------------------

// Middleware returns per-IP rate limiting for one surface. A refused request
// is answered through writeErr, in the format that surface's clients read, and
// audited as route.RateLimitRejected, where route is "s3" or "admin".
func (rl *RateLimiter) Middleware(route string, writeErr httputil.ErrorWriter) func(http.Handler) http.Handler {
	event := route + ".RateLimitRejected"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := rl.extractIP(r)
			if !rl.Allow(ip) {
				telemetry.RateLimitRejectionsTotal.Inc()
				audit.Log(r.Context(), event,
					slog.String("client_ip", ip),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
				)
				w.Header().Set("Retry-After", "1")
				writeErr(w, http.StatusTooManyRequests, "SlowDown", "Rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractIP gets the client IP from the request, trusting X-Forwarded-For only
// from the configured proxies.
func (rl *RateLimiter) extractIP(r *http.Request) string {
	return httputil.ExtractClientIP(r, rl.trustedProxies)
}
