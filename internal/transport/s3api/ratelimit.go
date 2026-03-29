// -------------------------------------------------------------------------------
// Rate Limiter - Per-IP Token Bucket Throttling
//
// Author: Alex Freidah
//
// Per-IP token bucket rate limiter with automatic cleanup of stale entries.
// When enabled, requests exceeding the configured rate receive 429 SlowDown.
// Supports X-Forwarded-For for accurate IP extraction behind trusted reverse
// proxies. When trusted_proxies is configured, only requests arriving from a
// trusted proxy CIDR will have their X-Forwarded-For header inspected; all
// other requests use RemoteAddr directly. The rightmost non-trusted IP in the
// XFF chain is used as the client IP, which is the standard secure extraction
// method (the rightmost entry appended by your trusted proxy).
// -------------------------------------------------------------------------------

package s3api

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"


	"golang.org/x/time/rate"

	"github.com/afreidah/s3-orchestrator/internal/observe/audit"
	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/transport/httputil"
	"github.com/afreidah/s3-orchestrator/internal/observe/telemetry"
)

// RateLimiter provides per-IP token-bucket rate limiting.
type RateLimiter struct {
	mu              sync.Mutex
	limiters        map[string]*visitorLimiter
	rate            rate.Limit
	burst           int
	trustedProxies  []*net.IPNet
	cleanupInterval time.Duration
	cleanupMaxAge   time.Duration
	stop            chan struct{}
	closeOnce       sync.Once
}

type visitorLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter creates a rate limiter with the given configuration.
func NewRateLimiter(cfg config.RateLimitConfig) *RateLimiter {
	cleanupInterval := cfg.CleanupInterval
	if cleanupInterval == 0 {
		cleanupInterval = 1 * time.Minute
	}
	cleanupMaxAge := cfg.CleanupMaxAge
	if cleanupMaxAge == 0 {
		cleanupMaxAge = 5 * time.Minute
	}

	rl := &RateLimiter{
		limiters:        make(map[string]*visitorLimiter),
		rate:            rate.Limit(cfg.RequestsPerSec),
		burst:           cfg.Burst,
		trustedProxies:  httputil.ParseTrustedProxies(cfg.TrustedProxies),
		cleanupInterval: cleanupInterval,
		cleanupMaxAge:   cleanupMaxAge,
		stop:            make(chan struct{}),
	}

	go func() {
		ticker := time.NewTicker(rl.cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.cleanup(rl.cleanupMaxAge)
			case <-rl.stop:
				return
			}
		}
	}()

	return rl
}

// Close stops the background cleanup goroutine. Safe to call multiple times.
func (rl *RateLimiter) Close() {
	rl.closeOnce.Do(func() {
		close(rl.stop)
	})
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

// Middleware wraps an http.Handler with per-IP rate limiting.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.extractIP(r)
		if !rl.Allow(ip) {
			telemetry.RateLimitRejectionsTotal.Inc()
			audit.Log(r.Context(), "s3.RateLimitRejected",
				slog.String("client_ip", ip),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			w.Header().Set("Retry-After", "1")
			writeS3Error(w, http.StatusTooManyRequests, "SlowDown", "Rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractIP gets the client IP from the request, delegating to ExtractClientIP.
func (rl *RateLimiter) extractIP(r *http.Request) string {
	return ExtractClientIP(r, rl.trustedProxies)
}

// ExtractClientIP delegates to httputil.ExtractClientIP.
func ExtractClientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	return httputil.ExtractClientIP(r, trustedProxies)
}

// ParseTrustedProxies delegates to httputil.ParseTrustedProxies.
func ParseTrustedProxies(cidrs []string) []*net.IPNet {
	return httputil.ParseTrustedProxies(cidrs)
}
