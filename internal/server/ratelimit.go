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

package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/afreidah/s3-orchestrator/internal/config"
	"github.com/afreidah/s3-orchestrator/internal/telemetry"
)

// RateLimiter provides per-IP token-bucket rate limiting.
type RateLimiter struct {
	mu             sync.Mutex
	limiters       map[string]*visitorLimiter
	rate           rate.Limit
	burst          int
	trustedProxies []*net.IPNet
	stop           chan struct{}
}

type visitorLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter creates a rate limiter with the given configuration.
func NewRateLimiter(cfg config.RateLimitConfig) *RateLimiter {
	rl := &RateLimiter{
		limiters:       make(map[string]*visitorLimiter),
		rate:           rate.Limit(cfg.RequestsPerSec),
		burst:          cfg.Burst,
		trustedProxies: parseCIDRs(cfg.TrustedProxies),
		stop:           make(chan struct{}),
	}

	// Background cleanup of stale entries every 3 minutes
	go func() {
		ticker := time.NewTicker(3 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.cleanup(10 * time.Minute)
			case <-rl.stop:
				return
			}
		}
	}()

	return rl
}

// Close stops the background cleanup goroutine.
func (rl *RateLimiter) Close() {
	close(rl.stop)
}

// UpdateLimits changes the rate and burst for new visitors. Existing per-IP
// limiters keep their old rates until they expire and are recreated.
func (rl *RateLimiter) UpdateLimits(requestsPerSec float64, burst int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.rate = rate.Limit(requestsPerSec)
	rl.burst = burst
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
			writeS3Error(w, http.StatusTooManyRequests, "SlowDown", "Rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractIP gets the client IP from the request. When trusted_proxies is
// configured and the direct peer matches a trusted CIDR, the rightmost
// untrusted IP in the X-Forwarded-For chain is used. Otherwise, RemoteAddr
// is used directly.
func (rl *RateLimiter) extractIP(r *http.Request) string {
	peerIP := stripPort(r.RemoteAddr)

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && len(rl.trustedProxies) > 0 && rl.isTrusted(peerIP) {
		return rightmostUntrusted(xff, rl.trustedProxies)
	}

	return peerIP
}

// rightmostUntrusted walks the XFF chain from right to left and returns the
// first IP that is not in the trusted set. This is the standard secure
// extraction: proxies append to the right, so the rightmost untrusted entry
// is the actual client IP as seen by the first trusted hop.
func rightmostUntrusted(xff string, trusted []*net.IPNet) string {
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		if !ipInNets(ip, trusted) {
			return ip
		}
	}

	// All IPs in the chain are trusted; use the leftmost as a fallback
	return strings.TrimSpace(parts[0])
}

// isTrusted checks whether the given IP falls within any trusted CIDR.
func (rl *RateLimiter) isTrusted(ip string) bool {
	return ipInNets(ip, rl.trustedProxies)
}

// ipInNets checks whether the given IP string falls within any of the
// provided CIDR networks.
func ipInNets(ipStr string, nets []*net.IPNet) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseCIDRs parses a list of CIDR strings into net.IPNet values, skipping
// any that fail to parse.
func parseCIDRs(cidrs []string) []*net.IPNet {
	var nets []*net.IPNet
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

// stripPort removes the port from a host:port address.
func stripPort(addr string) string {
	ip, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return ip
}
